package nacosrt

// materializeTables sandbox tests (design §10.2 M-2): the pack carries
// the FULL backend inventory — rows absent from a later pack are deleted
// (full-manifest semantics, including the enabled row).

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	db := openPackDB(t)
	st := openTestStore(t)
	rt, err := New(Config{
		Server: "http://nacos", Service: "woca-worker", Advertise: "127.0.0.1:9092",
		St: st, DB: db,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestMaterializeSandboxBackends(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	// First pack: two backends, one enabled.
	pack := &Pack{Version: packVersion, Generation: "g1", Sandboxes: []SandboxRow{
		{ID: "sb-1", Name: "a", Type: "contract-http", Endpoint: "http://a:1", TimeoutMs: 120000, Enabled: true, LastProbeOK: true},
		{ID: "sb-2", Name: "b", Type: "cfbridge", Endpoint: "http://b:2", TimeoutMs: 60000},
	}}
	if err := rt.apply(ctx, pack); err != nil {
		t.Fatalf("apply 2-row pack: %v", err)
	}
	var n int
	if err := rt.cfg.DB.QueryRow(`SELECT count(*) FROM sandbox_backends`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d want 2", n)
	}
	row, err := EnabledSandbox(rt.cfg.DB.DB)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.ID != "sb-1" || !row.LastProbeOK {
		t.Fatalf("enabled = %+v", row)
	}

	// Second pack: only sb-2 remains — sb-1 (the enabled row) must be
	// deleted (full-manifest semantics).
	pack2 := &Pack{Version: packVersion, Generation: "g2", Sandboxes: []SandboxRow{
		{ID: "sb-2", Name: "b", Type: "cfbridge", Endpoint: "http://b:2", TimeoutMs: 60000, Enabled: true},
	}}
	if err := rt.apply(ctx, pack2); err != nil {
		t.Fatalf("apply 1-row pack: %v", err)
	}
	if err := rt.cfg.DB.QueryRow(`SELECT count(*) FROM sandbox_backends`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows after shrink = %d want 1", n)
	}
	var gone int
	if err := rt.cfg.DB.QueryRow(`SELECT count(*) FROM sandbox_backends WHERE id = 'sb-1'`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Fatal("shrink did not delete the absent row")
	}
	row, err = EnabledSandbox(rt.cfg.DB.DB)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.ID != "sb-2" {
		t.Fatalf("enabled after shrink = %+v", row)
	}

	// Empty pack: the manifest is empty — every row goes.
	pack3 := &Pack{Version: packVersion, Generation: "g3"}
	if err := rt.apply(ctx, pack3); err != nil {
		t.Fatalf("apply empty pack: %v", err)
	}
	if err := rt.cfg.DB.QueryRow(`SELECT count(*) FROM sandbox_backends`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rows after empty pack = %d want 0", n)
	}
	row, err = EnabledSandbox(rt.cfg.DB.DB)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("enabled after empty pack = %+v want nil", row)
	}
}

// TestEnabledSandboxNone: no enabled row → nil, no error.
func TestEnabledSandboxNone(t *testing.T) {
	rt := newTestRuntime(t)
	// A pack with one disabled backend (the ensure-DDL creates the table).
	pack := &Pack{Version: packVersion, Generation: "g1", Sandboxes: []SandboxRow{
		{ID: "sb-x", Name: "x", Type: "contract-http", Endpoint: "http://x:1", TimeoutMs: 120000},
	}}
	if err := rt.apply(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	row, err := EnabledSandbox(rt.cfg.DB.DB)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("enabled = %+v want nil", row)
	}
}

// LastGeneration exposes the last applied pack's generation (the
// capability heartbeat rewrites the ack row with it between deliveries).
func TestLastGeneration(t *testing.T) {
	r := &Runtime{}
	if got := r.LastGeneration(); got != "" {
		t.Fatalf("empty runtime LastGeneration = %q, want \"\"", got)
	}
	r.pack.Store(&Pack{Version: 2, Generation: "g7"})
	if got := r.LastGeneration(); got != "g7" {
		t.Fatalf("LastGeneration = %q, want g7", got)
	}
}

// TestRuntimePackConcurrent: pack swaps (the subscription loop's apply,
// Loop's goroutine) race against LastGeneration readers (the scheduler's
// and worker's heartbeat goroutines call it every 10s) — the whole-pack
// pointer swap must be atomic. The old plain field made this a data race
// the -race detector flags here.
func TestRuntimePackConcurrent(t *testing.T) {
	rt, err := New(Config{
		Server: "http://nacos", Service: "woca-scheduler", Advertise: "127.0.0.1:8080",
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	for range 4 { // writers: watchLoop-shaped apply churn
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				if err := rt.apply(ctx, &Pack{Version: packVersion, Generation: "g-" + strconv.Itoa(i)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for range 4 { // readers: heartbeat-shaped generation polls
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if g := rt.LastGeneration(); g != "" && !strings.HasPrefix(g, "g-") {
					t.Errorf("LastGeneration = %q, want a g-<n> value", g)
					return
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
}
