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
	for i := 0; i < 3; i++ {
		if err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil }); err != nil {
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
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
		calls++
		if calls == 1 {
			return errors.New("process died") // 首次 Call 报错 → 摘除、重启重试一次
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
	for i := 0; i < lspMaxSpawnFailures; i++ {
		_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	}
	// 第 3 次：熔断冷却中，直接报错且不再 spawn
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
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
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	time.Sleep(150 * time.Millisecond) // 等回收
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
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

// 池负责一次握手：同一 key 连续两次 call 复用同一 server 且 initialize
// 恰好一次（旧的 per-call initialize 会让第二次复用必失败——真实 gopls
// 对二次 initialize 报协议错误）。
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
	for i := 0; i < 2; i++ {
		err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
			_, err := lspDiagnostics(context.Background(), tr, path)
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

// 握手失败视为 spawn 失败：计入熔断（连续失败后进入冷却，不再 spawn）。
func TestLSPPoolInitFailureFeedsBreaker(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		// initialize is unscripted on this transport, so the handshake fails.
		return &fakeTransport{responses: map[string]any{}}, nil
	}, lspInitialize)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := 0; i < lspMaxSpawnFailures; i++ {
		if err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil }); err == nil {
			t.Fatal("want handshake failure surfaced")
		}
	}
	// 熔断冷却中：直接报错且不再 spawn。
	if err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil }); err == nil {
		t.Fatal("want broken error during cooldown")
	}
	if n := spawns.Load(); n != int32(lspMaxSpawnFailures) {
		t.Fatalf("spawns = %d, want %d (circuit open)", n, lspMaxSpawnFailures)
	}
}

// 同一 server 上的并发 call 必须串行：didOpen/diagnostic/didClose 序列交错
// 时（同文件场景 A 的 didClose 可先于 B 的 diagnostic）语义就坏了。
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
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
				record()
				time.Sleep(10 * time.Millisecond) // 足够让并发裸奔时重叠
				cur.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if d := maxDepth.Load(); d != 1 {
		t.Fatalf("max concurrent fn depth on one server = %d, want 1", d)
	}
}

// acquire 复用必须刷新 lastUsed：entry 空闲到接近阈值后被复用、fn 执行中
// 越过阈值，idle reaper 不得关掉在用的 transport（否则复用即失败）。
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
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	// 空闲 350ms（阈值 500ms 内），随后复用并在 fn 内越过阈值：reaper 在
	// fn 期间的 tick（600ms 处，cutoff=100ms）若看旧 lastUsed 就会回收。
	time.Sleep(350 * time.Millisecond)
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
		time.Sleep(350 * time.Millisecond)
		return nil
	})
	// 复用成功且未被回收 → 仍是同一进程。
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1 (in-use entry must not be reaped)", n)
	}
}
