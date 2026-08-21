package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// openSharedLedger opens one ledger over a shared sqlite database.
func openSharedLedger(t *testing.T) *runstate.Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	l, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Phase 2 acceptance: a run whose instance died mid-flight is re-queued by
// another instance's reconciler, so the session is admissible again
// cross-instance (the executing side of adoption arrives with the Phase 3
// worker; at Phase 2 the ledger is the takeover seam).
func TestReconcilerAdoptsDeadInstanceRuns(t *testing.T) {
	ledger := openSharedLedger(t)

	// Instance A started a run and died: the row is running under A's lease.
	if err := ledger.Start(context.Background(), runstate.Run{
		RunID: "run-dead", SessionID: "sess-adopt", ThreadID: "thread-dead",
	}, "srv-A", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	a := New(Config{
		Agent: newScriptedAgent(t, "a-reply"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	b := New(Config{
		Agent: newScriptedAgent(t, "b-reply"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srvB := httptest.NewServer(b.Handler())
	defer srvB.Close()

	// The dead lease lapses; B's reconciler re-queues the run.
	time.Sleep(80 * time.Millisecond)
	if err := b.reconcileRunLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, ok, err := ledger.Row(context.Background(), "run-dead")
	if err != nil || !ok {
		t.Fatalf("row = ok=%v err=%v", ok, err)
	}
	if row.Status != runstate.StatusQueued {
		t.Fatalf("run not re-queued: %+v", row)
	}

	// The takeover seam: a claiming worker (the Phase 3 role) receives the
	// re-queued run under its own lease, and once it finishes, the session
	// admits a fresh run on B.
	claimed, err := ledger.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil || claimed == nil || claimed.RunID != "run-dead" {
		t.Fatalf("claim after reap = %+v err=%v", claimed, err)
	}
	if err := ledger.Finish(context.Background(), "run-dead", "worker-1", "done"); err != nil {
		t.Fatal(err)
	}
	runID := startAndAwaitRun(t, a, srvB, ledger, "sess-adopt", "after adoption", "done")
	if runID == "" {
		t.Fatal("no run completed after adoption")
	}
}
