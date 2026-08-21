package runstate

// Gap tests: validation rejections, the postgres tenant-tx prologue over a
// sqlite stand-in, and read accessor branches the lifecycle suites miss.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

func TestNewRejectsNilDB(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil database should be rejected")
	}
}

func TestLedgerValidationErrors(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()

	if err := l.PutEvent(ctx, "r", 1, "", "{}"); err == nil {
		t.Fatal("empty event kind should be rejected")
	}
	if err := l.Enqueue(ctx, Run{RunID: "", SessionID: "s"}); err == nil {
		t.Fatal("Enqueue without run id should be rejected")
	}
	if err := l.Enqueue(ctx, Run{RunID: "r", SessionID: ""}); err == nil {
		t.Fatal("Enqueue without session id should be rejected")
	}
	if _, err := l.Claim(ctx, "", time.Minute); err == nil {
		t.Fatal("Claim without worker id should be rejected")
	}
	if _, err := l.Append(ctx, "r", Event{Kind: ""}); err == nil {
		t.Fatal("Append without kind should be rejected")
	}
	if err := l.Start(ctx, Run{RunID: "", SessionID: "s"}, "w", time.Minute); err == nil {
		t.Fatal("Start without run id should be rejected")
	}
}

// TestRowAndReplayAccessors: Row hit/miss and Replay's default limit.
func TestRowAndReplayAccessors(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Start(ctx, Run{RunID: "r1", SessionID: "s1"}, "w1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, "r1", Event{Kind: "note", Payload: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, "r1", Event{Kind: "note", Payload: "p2"}); err != nil {
		t.Fatal(err)
	}

	run, ok, err := l.Row(ctx, "r1")
	if err != nil || !ok || run.RunID != "r1" || run.Status != StatusRunning || run.Owner != "w1" {
		t.Fatalf("Row = %+v %v %v", run, ok, err)
	}
	_, ok, err = l.Row(ctx, "ghost")
	if err != nil || ok {
		t.Fatalf("Row miss = %v %v %v", run, ok, err)
	}

	// limit <= 0 falls back to the default cap and returns everything.
	evs, err := l.Replay(ctx, "r1", 0, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("Replay default limit = %v %v", evs, err)
	}
}

func TestStartDuplicateSessionSurfacesConstraint(t *testing.T) {
	l := openLedger(t)
	ctx := context.Background()
	if err := l.Start(ctx, Run{RunID: "r1", SessionID: "s1"}, "w1", time.Minute); err != nil {
		t.Fatal(err)
	}
	err := l.Start(ctx, Run{RunID: "r2", SessionID: "s1"}, "w1", time.Minute)
	if err == nil || err.Error() != "runstate: session s1 already has an active run" {
		t.Fatalf("duplicate Start = %v", err)
	}
}

// TestClaimOnClosedDB: the sqlite claim transaction surfaces the pool error.
func TestClaimOnClosedDB(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(context.Background(), "w1", time.Minute); err == nil {
		t.Fatal("Claim on a closed DB should fail")
	}
}

// TestTenantTxPrologue: a postgres-dialect ledger over sqlite exercises the
// tenant transaction prologue — BeginTx failure on a closed pool, and the
// set_config rejection (sqlite has no such function).
func TestTenantTxPrologue(t *testing.T) {
	newPg := func(t *testing.T) (*Ledger, *wocadb.DB) {
		t.Helper()
		db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "pg.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		l := &Ledger{db: db, dialect: wocadb.DialectPostgres, now: time.Now}
		return l, db
	}
	tenant := tenancy.NewContext(context.Background(), tenancy.Context{TenantID: "t1", UserID: "u1"})

	t.Run("closed pool", func(t *testing.T) {
		l, db := newPg(t)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := l.PutEvent(tenant, "r", 1, "k", "p"); err == nil {
			t.Fatal("PutEvent with failed BeginTx should error")
		}
		if _, err := l.Replay(tenant, "r", 0, 1); err == nil {
			t.Fatal("Replay with failed BeginTx should error")
		}
		if _, _, err := l.Row(tenant, "r"); err == nil {
			t.Fatal("Row with failed BeginTx should error")
		}
	})

	t.Run("set_config unsupported", func(t *testing.T) {
		l, _ := newPg(t)
		if err := l.PutEvent(tenant, "r", 1, "k", "p"); err == nil {
			t.Fatal("PutEvent without the tenant GUC should error")
		}
		if _, err := l.Replay(tenant, "r", 0, 1); err == nil {
			t.Fatal("Replay without the tenant GUC should error")
		}
		if _, _, err := l.Row(tenant, "r"); err == nil {
			t.Fatal("Row without the tenant GUC should error")
		}
	})
}

func TestPlaceholderDialects(t *testing.T) {
	sqlite := &Ledger{dialect: wocadb.DialectSQLite}
	if got := sqlite.ph(0); got != "?" {
		t.Fatalf("sqlite ph = %q", got)
	}
	pg := &Ledger{dialect: wocadb.DialectPostgres}
	if got := pg.ph(2); got != "$3" {
		t.Fatalf("postgres ph = %q", got)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("nil error is not a violation")
	}
	for _, msg := range []string{
		"UNIQUE constraint failed: runs.run_id",
		"duplicate key value violates unique constraint \"runs_pkey\"",
		"pq: 23505",
	} {
		if !isUniqueViolation(errString(msg)) {
			t.Fatalf("%q should be a violation", msg)
		}
	}
	if isUniqueViolation(errString("no such table")) {
		t.Fatal("unrelated error misclassified")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
