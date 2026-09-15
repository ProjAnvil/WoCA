package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func poolKeyForTest() (string, []string) { return "/ws", []string{"fake-lsp"} }

func TestLSPPoolReuse(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for range 3 {
		if err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1 (pooled reuse)", n)
	}
}

func TestLSPPoolRestartOnCrash(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	calls := 0
	err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error {
		calls++
		if calls == 1 {
			return errors.New("process died") // first Call errors → evict, restart, retry once
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (one restart)", n)
	}
}

func TestLSPPoolBrokenCircuitBreaker(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return nil, errors.New("spawn failed")
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for range lspMaxSpawnFailures {
		_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	}
	// 3rd call: in breaker cooldown, errors immediately without spawning
	err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	if err == nil {
		t.Fatal("want broken error during cooldown")
	}
	if n := spawns.Load(); n != int32(lspMaxSpawnFailures) {
		t.Fatalf("spawns = %d, want %d (circuit open)", n, lspMaxSpawnFailures)
	}
}

func TestLSPPoolIdleReap(t *testing.T) {
	oldIdle, oldTick := lspIdleTimeout, lspReapInterval
	lspIdleTimeout, lspReapInterval = 30*time.Millisecond, 10*time.Millisecond
	defer func() { lspIdleTimeout, lspReapInterval = oldIdle, oldTick }()
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	time.Sleep(150 * time.Millisecond) // wait for the reaper
	_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (idle entry reaped)", n)
	}
}

// countingInitTransport wraps a transport and counts initialize requests.
type countingInitTransport struct {
	lspTransport
	inits atomic.Int32
}

func (c *countingInitTransport) Call(ctx context.Context, method string, params, result any) error {
	if method == "initialize" {
		c.inits.Add(1)
	}
	return c.lspTransport.Call(ctx, method, params, result)
}

// The pool owns the single handshake: two consecutive calls on the same key
// reuse the same server with exactly one initialize (the old per-call
// initialize made every second reuse fail — real gopls reports a protocol
// error for a second initialize).
func TestLSPPoolInitializesOncePerServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var spawned *countingInitTransport
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawned = &countingInitTransport{lspTransport: &fakeTransport{responses: map[string]any{
			"initialize":              map[string]any{},
			"textDocument/diagnostic": map[string]any{"kind": "full", "items": []map[string]any{}},
		}}}
		return spawned, nil
	}, lspInitialize)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := range 2 {
		err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error {
			_, err := lspDiagnostics(t.Context(), tr, path)
			return err
		})
		if err != nil {
			t.Fatalf("pooled call %d: %v", i+1, err)
		}
	}
	if n := spawned.inits.Load(); n != 1 {
		t.Fatalf("initialize requests = %d, want 1 (pool owns the handshake)", n)
	}
}

// A handshake failure counts as a spawn failure: it feeds the breaker
// (after consecutive failures it enters cooldown and stops spawning).
func TestLSPPoolInitFailureFeedsBreaker(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		// initialize is unscripted on this transport, so the handshake fails.
		return &fakeTransport{responses: map[string]any{}}, nil
	}, lspInitialize)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for range lspMaxSpawnFailures {
		if err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil }); err == nil {
			t.Fatal("want handshake failure surfaced")
		}
	}
	// In breaker cooldown: errors immediately without spawning.
	if err := p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil }); err == nil {
		t.Fatal("want broken error during cooldown")
	}
	if n := spawns.Load(); n != int32(lspMaxSpawnFailures) {
		t.Fatalf("spawns = %d, want %d (circuit open)", n, lspMaxSpawnFailures)
	}
}

// Concurrent calls on the same server must serialize: if
// didOpen/diagnostic/didClose sequences interleave (in a same-file scenario
// A's didClose can precede B's diagnostic), the semantics break.
func TestLSPPoolPerEntrySerialization(t *testing.T) {
	var cur, maxDepth atomic.Int32
	record := func() {
		n := cur.Add(1)
		for {
			m := maxDepth.Load()
			if n <= m || maxDepth.CompareAndSwap(m, n) {
				break
			}
		}
	}
	p := newLSPPool(func([]string) (lspTransport, error) {
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error {
				record()
				time.Sleep(10 * time.Millisecond) // long enough to overlap if calls ran unserialized
				cur.Add(-1)
				return nil
			})
		})
	}
	wg.Wait()
	if d := maxDepth.Load(); d != 1 {
		t.Fatalf("max concurrent fn depth on one server = %d, want 1", d)
	}
}

// acquire-reuse must refresh lastUsed: an entry idle close to the threshold
// gets reused and fn crosses the threshold mid-run; the idle reaper must not
// close an in-use transport (otherwise the reuse fails).
func TestLSPPoolAcquireReuseRefreshesLastUsed(t *testing.T) {
	oldIdle, oldTick := lspIdleTimeout, lspReapInterval
	lspIdleTimeout, lspReapInterval = 500*time.Millisecond, 100*time.Millisecond
	defer func() { lspIdleTimeout, lspReapInterval = oldIdle, oldTick }()
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	// Idle for 350ms (within the 500ms threshold), then reused and crossing
	// the threshold inside fn: a reaper tick during fn (at 600ms, cutoff=100ms)
	// would reap the entry if it saw the stale lastUsed.
	time.Sleep(350 * time.Millisecond)
	_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error {
		time.Sleep(350 * time.Millisecond)
		return nil
	})
	// Reuse succeeded without being reaped → still the same process.
	_ = p.call(t.Context(), ws, cmd, func(tr lspTransport) error { return nil })
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1 (in-use entry must not be reaped)", n)
	}
}
