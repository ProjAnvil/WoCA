package runstate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

func openLedger(t *testing.T) *Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestEnqueueClaimFinishLifecycle(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()

	if err := l.Enqueue(ctx, Run{RunID: "r1", SessionID: "s1", ConfigSnapshot: `{"model":"m"}`}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := l.ActiveRun(ctx, "s1")
	if err != nil || !ok || got.RunID != "r1" || got.Status != "queued" {
		t.Fatalf("ActiveRun = %+v ok=%v err=%v", got, ok, err)
	}

	claimed, err := l.Claim(ctx, "worker-a", time.Minute)
	if err != nil || claimed == nil || claimed.RunID != "r1" {
		t.Fatalf("Claim = %+v err=%v", claimed, err)
	}
	if claimed.Owner != "worker-a" || claimed.Status != "running" {
		t.Fatalf("claimed = %+v", claimed)
	}

	if err := l.Heartbeat(ctx, "r1", "worker-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A different worker cannot extend the lease.
	if err := l.Heartbeat(ctx, "r1", "worker-b", time.Minute); err == nil {
		t.Fatal("foreign heartbeat accepted")
	}

	if err := l.Finish(ctx, "r1", "worker-a", "done"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := l.ActiveRun(ctx, "s1"); err != nil || got != nil {
		t.Fatalf("run active after finish: %+v err=%v", got, err)
	}
	// Terminal runs cannot be finished again or by another worker.
	if err := l.Finish(ctx, "r1", "worker-a", "error"); err == nil {
		t.Fatal("double finish accepted")
	}
}

func TestOneActiveRunPerSessionConstraint(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()

	if err := l.Enqueue(ctx, Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r2", SessionID: "s1"}); err == nil {
		t.Fatal("second active run for one session accepted")
	}
	// After the first finishes the session admits a new run.
	if _, err := l.Claim(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(ctx, "r1", "w", "done"); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r2", SessionID: "s1"}); err != nil {
		t.Fatalf("enqueue after finish: %v", err)
	}
}

func TestClaimFIFOAndLeaseExpiryAdoption(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	l.now = func() time.Time { return time.UnixMilli(1000) }

	if err := l.Enqueue(ctx, Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r2", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	first, err := l.Claim(ctx, "worker-a", time.Minute)
	if err != nil || first.RunID != "r1" {
		t.Fatalf("FIFO claim = %+v err=%v", first, err)
	}

	// Time passes beyond the lease: another worker adopts r1; r2 stays queued.
	l.now = func() time.Time { return time.UnixMilli(1000).Add(2 * time.Minute) }
	adopted, err := l.ReapExpired(ctx)
	if err != nil || len(adopted) != 1 || adopted[0] != "r1" {
		t.Fatalf("ReapExpired = %v err=%v", adopted, err)
	}
	next, err := l.Claim(ctx, "worker-b", time.Minute)
	if err != nil || next == nil {
		t.Fatal(err)
	}
	// r2 queued first and never ran, so FIFO hands it out before the
	// re-queued r1; the adopted r1 stays next in line.
	if next.RunID != "r2" || next.Owner != "worker-b" {
		t.Fatalf("post-reap claim = %+v", next)
	}
}

func TestAppendAndReplayEvents(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Enqueue(ctx, Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}

	for _, ev := range []Event{
		{Kind: "llm_call", Payload: `{"model":"m"}`},
		{Kind: "tool_call", Payload: `{"name":"Bash"}`},
		{Kind: "tool_result", Payload: `{"ok":true}`},
	} {
		if _, err := l.Append(ctx, "r1", ev); err != nil {
			t.Fatal(err)
		}
	}
	all, err := l.Replay(ctx, "r1", 0, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("Replay = %d err=%v", len(all), err)
	}
	if all[0].Seq != 1 || all[1].Seq != 2 || all[2].Seq != 3 {
		t.Fatalf("seq order = %+v", all)
	}
	// sinceSeq resumes strictly after the cursor.
	tail, err := l.Replay(ctx, "r1", 2, 10)
	if err != nil || len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("tail Replay = %+v err=%v", tail, err)
	}
	// Events on an unknown run are rejected.
	if _, err := l.Append(ctx, "missing", Event{Kind: "x"}); err == nil {
		t.Fatal("append to unknown run accepted")
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	l := openLedger(t)
	if got, err := l.Claim(context.Background(), "w", time.Minute); err != nil || got != nil {
		t.Fatalf("Claim on empty = %+v err=%v", got, err)
	}
}
