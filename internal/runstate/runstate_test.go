package runstate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

func TestPutEventExplicitSeqSemantics(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Enqueue(ctx, Run{RunID: "r", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := l.PutEvent(ctx, "r", 5, "delta", `{}`); err != nil {
		t.Fatal(err)
	}
	// Idempotent retry of the same (run, seq, kind).
	if err := l.PutEvent(ctx, "r", 5, "delta", `{}`); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	// Same seq with a different kind is a conflict, not a silent overwrite.
	if err := l.PutEvent(ctx, "r", 5, "tool_call", `{}`); err == nil {
		t.Fatal("conflicting kind at same seq accepted")
	}
	if err := l.PutEvent(ctx, "r", 1, "", "{}"); err == nil {
		t.Fatal("empty kind accepted")
	}
	// The explicit seq does not disturb Append's monotonic minting.
	if _, err := l.Append(ctx, "r", Event{Kind: "tail"}); err != nil {
		t.Fatal(err)
	}
	tail, _ := l.Replay(ctx, "r", 4, 10)
	if len(tail) != 2 || tail[0].Seq != 5 || tail[1].Seq != 6 {
		t.Fatalf("events after explicit seq = %+v", tail)
	}
}

// --- Task 14: the workspace_id execution-side channel + claim mutex ---

// TestEnqueuePersistsWorkspaceID: the workspace identity a dispatched run
// inherits from its session lands in runs.workspace_id — the execution-side
// channel the worker reads (Run.WorkspaceID); session metadata never
// carries it across the queue.
func TestEnqueuePersistsWorkspaceID(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Enqueue(ctx, Run{RunID: "r-ws", SessionID: "s1", WorkspaceID: "ws-9"}); err != nil {
		t.Fatal(err)
	}
	var wsID string
	if err := l.db.QueryRow(`SELECT workspace_id FROM runs WHERE run_id = 'r-ws'`).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	if wsID != "ws-9" {
		t.Fatalf("runs.workspace_id = %q, want ws-9", wsID)
	}
}

// TestLegacyRunsTableUpgradesForWorkspaceID: a database created by a
// pre-sandbox-plane build (runs without workspace_id) upgrades on Open, and
// enqueueing a run that carries a workspace identity does not fail with
// "no such column".
func TestLegacyRunsTableUpgradesForWorkspaceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			status TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			lease_expires_at BIGINT NOT NULL DEFAULT 0,
			thread_id TEXT NOT NULL DEFAULT '',
			config_snapshot TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := wocadb.Open("sqlite:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(context.Background(), Run{RunID: "r1", SessionID: "s1", WorkspaceID: "ws-1"}); err != nil {
		t.Fatalf("enqueue on upgraded legacy db: %v", err)
	}
}

// TestClaimSkipsRunsOfRunningWorkspace: the per-workspace mutex at claim
// time — while one run of a workspace is running, a queued run of the SAME
// workspace is not a claim candidate (an unrelated queued run claims
// instead), and once the running run finishes the held run claims cleanly.
func TestClaimSkipsRunsOfRunningWorkspace(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	// r1 runs on ws-1; r2 queues behind it on ws-1; r3 queues on ws-2.
	if err := l.Start(ctx, Run{RunID: "r1", SessionID: "s1", WorkspaceID: "ws-1"}, "w0", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r2", SessionID: "s2", WorkspaceID: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r3", SessionID: "s3", WorkspaceID: "ws-2"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.Claim(ctx, "w1", time.Minute)
	if err != nil || got == nil {
		t.Fatalf("claim = %v err=%v", got, err)
	}
	if got.RunID != "r3" {
		t.Fatalf("claim picked %q, want the unrelated r3 (r2's workspace is mutex-held)", got.RunID)
	}
	if got.WorkspaceID != "ws-2" {
		t.Fatalf("claimed run WorkspaceID = %q, want ws-2 (execution-side channel)", got.WorkspaceID)
	}
	if err := l.Finish(ctx, "r1", "w0", "done"); err != nil {
		t.Fatal(err)
	}
	next, err := l.Claim(ctx, "w2", time.Minute)
	if err != nil || next == nil || next.RunID != "r2" {
		t.Fatalf("claim after release = %+v err=%v", next, err)
	}
	if next.WorkspaceID != "ws-1" {
		t.Fatalf("released claim WorkspaceID = %q, want ws-1", next.WorkspaceID)
	}
}

// TestClaimWorkspaceMutexCollisionIsRetryable: the residual claim race (two
// workers selecting the same queued row under READ COMMITTED snapshots) is
// caught by the one_running_run_per_workspace partial index at UPDATE time
// on postgres; sqlite serializes writes so the equivalent path is simulated
// here — claim's own UPDATE, aimed at a workspace that already has a
// running run, fails with the unique violation the retry classification
// recognizes, the row stays queued, and a retry after the running run
// finishes claims it. That is exactly the contract Worker.Run's 1s backoff
// relies on (claim may safely return the error).
func TestClaimWorkspaceMutexCollisionIsRetryable(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Start(ctx, Run{RunID: "r1", SessionID: "s1", WorkspaceID: "ws-1"}, "w0", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "r2", SessionID: "s2", WorkspaceID: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	// The losing side of the race: its candidate snapshot predates r1's
	// commit, so its UPDATE targets r2 while r1 already holds ws-1.
	_, err := l.db.Exec(`UPDATE runs SET status = 'running', owner = 'w1', lease_expires_at = 0, updated_at = 0
		WHERE run_id = 'r2' AND status = 'queued'`)
	if err == nil {
		t.Fatal("the workspace-mutex UPDATE must fail while another run holds the workspace")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("collision error not retry-classified: %v", err)
	}
	// The loser left no damage: r2 is still queued and claims after release.
	var status string
	if err := l.db.QueryRow(`SELECT status FROM runs WHERE run_id = 'r2'`).Scan(&status); err != nil || status != StatusQueued {
		t.Fatalf("r2 status after failed claim = %q err=%v (want queued)", status, err)
	}
	if err := l.Finish(ctx, "r1", "w0", "done"); err != nil {
		t.Fatal(err)
	}
	got, err := l.Claim(ctx, "w2", time.Minute)
	if err != nil || got == nil || got.RunID != "r2" {
		t.Fatalf("retry claim = %+v err=%v", got, err)
	}
}

// TestClaimSQLExcludesRunningWorkspaces: SQL shape review for both dialect
// branches — the candidate selection carries the NOT EXISTS per-workspace
// mutex, and the workspace_id column rides the claim projection (RETURNING
// on postgres, SELECT on sqlite) so the worker's Run.WorkspaceID has an
// execution-side source.
func TestClaimSQLExcludesRunningWorkspaces(t *testing.T) {
	pg := &Ledger{dialect: wocadb.DialectPostgres, now: time.Now}
	sqlite := &Ledger{dialect: wocadb.DialectSQLite, now: time.Now}
	for _, tc := range []struct {
		name, q string
	}{
		{"postgres update", pg.claimUpdateSQL()},
		{"sqlite select", sqlite.claimSelectSQL()},
	} {
		if !strings.Contains(tc.q, "NOT EXISTS (SELECT 1 FROM runs r2") {
			t.Errorf("%s: no per-workspace mutex subquery: %s", tc.name, tc.q)
		}
		if !strings.Contains(tc.q, "r2.workspace_id = runs.workspace_id") ||
			!strings.Contains(tc.q, "r2.workspace_id <> ''") ||
			!strings.Contains(tc.q, "r2.status='"+StatusRunning+"'") {
			t.Errorf("%s: mutex subquery malformed: %s", tc.name, tc.q)
		}
		if !strings.Contains(tc.q, "workspace_id") {
			t.Errorf("%s: claim projection misses workspace_id: %s", tc.name, tc.q)
		}
	}
}

// A closed database surfaces errors on every path instead of panicking.
func TestLedgerClosedDBSurfacesErrors(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Enqueue(ctx, Run{RunID: "r", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := l.db.Close(); err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error on closed db", what)
		}
	}
	check("enqueue", l.Enqueue(ctx, Run{RunID: "x", SessionID: "y"}))
	check("start", l.Start(ctx, Run{RunID: "x", SessionID: "y"}, "w", time.Minute))
	_, err := l.Claim(ctx, "w", time.Minute)
	check("claim", err)
	check("heartbeat", l.Heartbeat(ctx, "r", "w", time.Minute))
	check("finish", l.Finish(ctx, "r", "w", "done"))
	check("append", func() error { _, err := l.Append(ctx, "r", Event{Kind: "k"}); return err }())
	check("putevent", l.PutEvent(ctx, "r", 1, "k", "{}"))
	_, _, err = l.ActiveRun(ctx, "s")
	check("activerun", err)
	_, err = l.ReapExpired(ctx)
	check("reap", err)
	_, err = l.Replay(ctx, "r", 0, 5)
	check("replay", err)
	_, _, err = l.Row(ctx, "r")
	check("row", err)
}
