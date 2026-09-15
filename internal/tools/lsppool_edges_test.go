package tools

import (
	"bufio"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	// Registering the BMP decoder (from the already-required x/image module)
	// lets ProcessImage reach its unsupported-format branch.
	_ "golang.org/x/image/bmp"
)

// --- lspPool edge branches ---

// TestLSPPoolClosedGuards covers acquire on a stopped pool and the
// double-Close no-op.
func TestLSPPoolClosedGuards(t *testing.T) {
	p := newLSPPool(func([]string) (lspTransport, error) {
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	p.Close()
	p.Close() // second Close must be a no-op, not panic

	ws, cmd := poolKeyForTest()
	err := p.call(t.Context(), ws, cmd, func(lspTransport) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "pool closed") {
		t.Fatalf("call after Close = %v, want pool closed error", err)
	}
}

// TestLSPPoolRestartAcquireFails: the first run fails (evict + restart), and
// the restart's spawn fails, surfacing the acquire error from the retry.
func TestLSPPoolRestartAcquireFails(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		if spawns.Add(1) > 1 {
			return nil, errors.New("spawn broke")
		}
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	err := p.call(t.Context(), ws, cmd, func(lspTransport) error {
		return errors.New("call failed") // force evict and retry
	})
	if err == nil || !strings.Contains(err.Error(), "spawn broke") {
		t.Fatalf("retry acquire = %v, want spawn broke", err)
	}
}

// TestLSPPoolRestartFailsAgainEvicts: when the retried run also fails, the
// second transport is evicted too, so a later call spawns fresh again.
func TestLSPPoolRestartFailsAgainEvicts(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	if err := p.call(t.Context(), ws, cmd, func(lspTransport) error {
		return errors.New("still failing")
	}); err == nil {
		t.Fatal("second failure should be returned")
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2", n)
	}
	if err := p.call(t.Context(), ws, cmd, func(lspTransport) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := spawns.Load(); n != 3 {
		t.Fatalf("spawns = %d, want 3 (entry evicted after second failure)", n)
	}
}

// closeCountingTransport counts Close calls on a wrapped transport.
type closeCountingTransport struct {
	lspTransport
	closes *atomic.Int32
}

func (c *closeCountingTransport) Close() error {
	c.closes.Add(1)
	return c.lspTransport.Close()
}

// TestLSPPoolConcurrentSpawnKeepsFirstTransport: two calls race the spawn for
// one key; both spawns complete (barrier), and the entry keeps the transport
// that entered the pool first while the loser's is closed.
func TestLSPPoolConcurrentSpawnKeepsFirstTransport(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var closes atomic.Int32
	spawn := func([]string) (lspTransport, error) {
		arrived <- struct{}{}
		<-release
		return &closeCountingTransport{
			lspTransport: &fakeTransport{responses: map[string]any{}},
			closes:       &closes,
		}, nil
	}
	p := newLSPPool(spawn, nil)

	ws, cmd := poolKeyForTest()
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- p.call(t.Context(), ws, cmd, func(lspTransport) error { return nil })
		}()
	}
	<-arrived
	<-arrived // both spawns in flight before either can commit
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if active, _ := p.stats(); active != 1 {
		t.Fatalf("active = %d, want 1 pooled entry", active)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("closes = %d, want 1 (loser transport discarded)", n)
	}
	p.Close()
	if n := closes.Load(); n != 2 {
		t.Fatalf("closes after pool Close = %d, want 2", n)
	}
}

// TestLSPPoolStatsReportsBroken: once the breaker opens, stats counts the
// entry as broken and not active.
func TestLSPPoolStatsReportsBroken(t *testing.T) {
	p := newLSPPool(func([]string) (lspTransport, error) {
		return nil, errors.New("spawn failed")
	}, nil)
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for range lspMaxSpawnFailures {
		_ = p.call(t.Context(), ws, cmd, func(lspTransport) error { return nil })
	}
	active, broken := p.stats()
	if active != 0 || broken != 1 {
		t.Fatalf("stats = (%d active, %d broken), want (0, 1)", active, broken)
	}
}

// --- lsp stdioTransport.failAll ---

type failAllReader struct{}

func (failAllReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestLSPStdioFailAllDeliversPendingError: a reader that fails immediately
// makes readLoop fail every pending call with the read error.
func TestLSPStdioFailAllDeliversPendingError(t *testing.T) {
	tr := &stdioTransport{
		stdin:   &jsonWriter{w: nopWriteCloser{}},
		stdout:  bufio.NewReader(failAllReader{}),
		pending: map[int64]chan lspResponse{},
	}
	ch := make(chan lspResponse, 1)
	tr.pending[42] = ch
	go tr.readLoop()
	select {
	case resp := <-ch:
		if resp.Error == nil || !strings.Contains(resp.Error.Message, io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("pending response = %+v, want read error", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call was not failed after reader death")
	}
}

// --- registry ---

// TestRegistryLSPStatsWithoutPool covers the nil-pool guard in LSPStats.
func TestRegistryLSPStatsWithoutPool(t *testing.T) {
	r := &Registry{}
	if active, broken := r.LSPStats(); active != 0 || broken != 0 {
		t.Fatalf("LSPStats without pool = (%d, %d), want zeros", active, broken)
	}
}

// --- background shells constructors ---

// TestNewBackgroundShellsDefault covers the local-machine constructors.
func TestNewBackgroundShellsDefault(t *testing.T) {
	bg := NewBackgroundShells()
	if bg == nil {
		t.Fatal("NewBackgroundShells returned nil")
	}
	if got := bg.List(); len(got) != 0 {
		t.Fatalf("new manager has %d shells, want 0", len(got))
	}
}

// --- web helpers ---

// TestCleanDuckDuckGoURLDoubleUnescapeFails: uddg=%25zz unescapes once to
// "%zz"; the second unescape fails, so the raw (once-unescaped) target is
// returned unchanged.
func TestCleanDuckDuckGoURLDoubleUnescapeFails(t *testing.T) {
	if got := cleanDuckDuckGoURL("//duckduckgo.com/l/?uddg=%25zz"); got != "%zz" {
		t.Fatalf("cleanDuckDuckGoURL = %q, want %%zz", got)
	}
}

// --- image ---

// TestProcessImageUnsupportedFormat: a valid 1x1 BMP decodes via
// image.DecodeConfig (the bmp driver registered below) but has no MIME
// mapping in mimeByFormat, so ProcessImage rejects it as unsupported.
func TestProcessImageUnsupportedFormat(t *testing.T) {
	bmpBytes, err := hex.DecodeString("424d3a00000000000000360000002800000001000000010000000100180000000000040000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.bmp")
	if err := os.WriteFile(path, bmpBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ProcessImage(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported image format") {
		t.Fatalf("err = %v, want unsupported image format", err)
	}
}
