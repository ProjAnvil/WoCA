package runstate

import (
	"context"
	"database/sql"
	"fmt"
	neturl "net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/projanvil/woca/internal/wocadb"
)

// TestLedgerOnPostgres exercises the postgres-specific paths (UPDATE…
// RETURNING claim, reap, finish, constraints) against a live server. Set
// WOCA_PG_TEST_DSN (any database; the test creates and drops its own).
// Tenant-scoped visibility of these tables is proven by the wocadb
// ownership test (T7).
func TestLedgerOnPostgres(t *testing.T) {
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_rl_%d", os.Getpid())
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP DATABASE `+dbName) })
	defer admin.Close()

	dsn := dsnForDatabase(baseDSN, dbName)
	db, err := wocadb.Open("pg:" + dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Enqueue(ctx, Run{RunID: "pg-1", SessionID: "s1", ThreadID: "th1"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := l.Claim(ctx, "w-a", time.Minute)
	if err != nil || claimed == nil || claimed.RunID != "pg-1" || claimed.ThreadID != "th1" {
		t.Fatalf("pg claim = %+v err=%v", claimed, err)
	}
	if err := l.Heartbeat(ctx, "pg-1", "w-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, "pg-1", Event{Kind: "k", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	evs, err := l.Replay(ctx, "pg-1", 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("pg replay = %v err=%v", evs, err)
	}
	row, ok, err := l.Row(ctx, "pg-1")
	if err != nil || !ok || row.Owner != "w-a" {
		t.Fatalf("pg row = %+v ok=%v err=%v", row, ok, err)
	}
	if err := l.Finish(ctx, "pg-1", "w-a", "done"); err != nil {
		t.Fatal(err)
	}

	// Lease expiry reap on postgres: a lapsed running row re-queues.
	if err := l.Enqueue(ctx, Run{RunID: "pg-2", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.UnixMilli(1000) }
	if _, err := l.Claim(ctx, "w-b", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.UnixMilli(1000).Add(time.Second) }
	reaped, err := l.ReapExpired(ctx)
	if err != nil || len(reaped) != 1 || reaped[0] != "pg-2" {
		t.Fatalf("pg reap = %v err=%v", reaped, err)
	}
	next, err := l.Claim(ctx, "w-c", time.Minute)
	if err != nil || next == nil || next.RunID != "pg-2" || next.Owner != "w-c" {
		t.Fatalf("pg readopt = %+v err=%v", next, err)
	}

	// Constraint semantics under postgres.
	if err := l.Start(ctx, Run{RunID: "pg-3", SessionID: "s3"}, "w-d", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(ctx, Run{RunID: "pg-3b", SessionID: "s3"}, "w-d", time.Minute); err == nil {
		t.Fatal("second active run for session accepted")
	}
	if _, err := l.Append(ctx, "missing", Event{Kind: "x"}); err == nil {
		t.Fatal("append to unknown run accepted")
	}
}

// TestRacingClaimsOnPostgres is the regression for the double-claim bug
// observed on the live full stack: two workers racing Claim on one queued
// row — the loser's subquery snapshot still sees it queued, so without the
// status re-check in the outer UPDATE WHERE, EvalPlanQual let it steal the
// already-running row and both executed the run.
func TestRacingClaimsOnPostgres(t *testing.T) {
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_rl_race_%d", os.Getpid())
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP DATABASE `+dbName) })
	defer admin.Close()

	dsn := dsnForDatabase(baseDSN, dbName)
	db, err := wocadb.Open("pg:" + dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Enqueue(ctx, Run{RunID: "race-1", SessionID: "sr", ThreadID: "thr"}); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	start := make(chan struct{})
	results := make(chan *Run, racers)
	for i := range racers {
		go func(i int) {
			<-start
			run, err := l.Claim(ctx, fmt.Sprintf("w-%d", i), time.Minute)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				run = nil
			}
			results <- run
		}(i)
	}
	close(start)
	winners := 0
	for range racers {
		if r := <-results; r != nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d workers claimed the same queued run (want exactly 1)", winners)
	}
}

// dsnForDatabase swaps the database name in the test DSN so the live tests
// run against any deployment form (host-published base stack, or an
// in-network address like woca-pg-primary:5432).
func dsnForDatabase(baseDSN, dbName string) string {
	u, err := neturl.Parse(baseDSN)
	if err != nil {
		return fmt.Sprintf("postgres://woca:woca-enterprise-dev@127.0.0.1:35432/%s?sslmode=disable", dbName)
	}
	u.Path = "/" + dbName
	return u.String()
}
