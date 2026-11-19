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
	"github.com/projanvil/woca/internal/platform/tenancy"
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

// TestTenantPropagationOnPostgres is the T6 regression: a run enqueued
// under a tenant-carrying context is tagged with that tenant, a tenantless
// worker Claim hands the tenant back on the run, events appended under the
// restored tenant land tenant-scoped, and the RLS policy keeps them visible
// only to that tenant's readers (a foreign tenant — or the tenantless
// default — sees nothing). Pre-fix, the enqueue defaulted to 'local' and
// the tenant's own SSE replay came back empty.
//
// RLS only binds a non-superuser owner, so the ledger runs as a dedicated
// NOBYPASSRLS app role over its own throwaway database — the same shape
// wocadb's rls_pg_test provisions (a plain superuser connection bypasses
// the policies entirely and would vacuously pass).
func TestTenantPropagationOnPostgres(t *testing.T) {
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_rl_tenant_%d", os.Getpid())
	role, pass := dbName+"_app", "tenant-test"
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS`, role, pass)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `GRANT ALL ON DATABASE `+dbName+` TO `+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+role)
	})
	defer admin.Close()

	// PG 15+ keeps CREATE on public from ordinary roles; grant it before
	// the migrations run as the app role.
	grant, err := sql.Open("pgx", dsnForDatabase(baseDSN, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.ExecContext(ctx, `GRANT CREATE ON SCHEMA public TO `+role); err != nil {
		_ = grant.Close()
		t.Fatal(err)
	}
	_ = grant.Close()

	appDSN := rewriteUserPassword(dsnForDatabase(baseDSN, dbName), role, pass)
	db, err := wocadb.Open("pg:" + appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Enqueue carries the tenant: the gateway-side request context tags
	// the runs row with the originating tenant.
	gwCtx := tenancy.NewContext(ctx, tenancy.Context{TenantID: "acme", UserID: "u1"})
	if err := l.Enqueue(gwCtx, Run{RunID: "t-1", SessionID: "st", ThreadID: "th"}); err != nil {
		t.Fatal(err)
	}
	var tenantID string
	if err := db.QueryRowContext(ctx, `SELECT tenant_id FROM runs WHERE run_id = 't-1'`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if tenantID != "acme" {
		t.Fatalf("runs.tenant_id = %q, want acme (enqueue dropped the request tenant)", tenantID)
	}

	// 2. The worker claims without any tenant in its context; the claimed
	// run carries the tenant it was enqueued under.
	claimed, err := l.Claim(ctx, "w-tenant", time.Minute)
	if err != nil || claimed == nil || claimed.RunID != "t-1" {
		t.Fatalf("claim = %+v err=%v", claimed, err)
	}
	if claimed.TenantID != "acme" {
		t.Fatalf("claimed run TenantID = %q, want acme", claimed.TenantID)
	}

	// 3. Worker-side events append under the restored tenant and stay
	// visible to the owning tenant's replay — the SSE read path.
	wCtx := tenancy.NewContext(ctx, tenancy.Context{TenantID: claimed.TenantID})
	if _, err := l.Append(wCtx, "t-1", Event{Kind: "delta", Payload: `{"text":"hi"}`}); err != nil {
		t.Fatal(err)
	}
	own, err := l.Replay(gwCtx, "t-1", 0, 10)
	if err != nil || len(own) != 1 {
		t.Fatalf("tenant replay = %v err=%v (worker events invisible to their own tenant)", own, err)
	}

	// 4. RLS isolation: a different tenant — and the tenantless default
	// that pre-fix received the row — see none of the tenant's events.
	other := tenancy.NewContext(ctx, tenancy.Context{TenantID: "other"})
	foreign, err := l.Replay(other, "t-1", 0, 10)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign tenant replay = %v err=%v, want isolation", foreign, err)
	}
	local, err := l.Replay(ctx, "t-1", 0, 10)
	if err != nil || len(local) != 0 {
		t.Fatalf("local replay = %v err=%v, want isolation", local, err)
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

// rewriteUserPassword swaps the user:password in a URL DSN (the app-role
// connection the tenant test migrates and runs as).
func rewriteUserPassword(dsn, user, pass string) string {
	u, err := neturl.Parse(dsn)
	if err != nil {
		return dsn
	}
	if u.User == nil {
		u.User = neturl.UserPassword(user, pass)
		return u.String()
	}
	u.User = neturl.UserPassword(user, pass)
	return u.String()
}

// TestSessionTenantOnPostgres: the session-tenant derivation the wake
// driver relies on — a session's most recently updated run row carries the
// tenant its wake-side work belongs to, sessions of the 'local' default (or
// with no runs) report none, and a later run under a different tenant wins
// (sessions are single-tenant in practice; the ledger records the truth).
func TestSessionTenantOnPostgres(t *testing.T) {
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_rl_st_%d", os.Getpid())
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName) })
	defer admin.Close()

	db, err := wocadb.Open("pg:" + dsnForDatabase(baseDSN, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := New(db)
	if err != nil {
		t.Fatal(err)
	}

	runTo := func(tenant, runID, sessionID string) {
		t.Helper()
		c := context.Background()
		if tenant != "" {
			c = tenancy.NewContext(c, tenancy.Context{TenantID: tenant})
		}
		if err := l.Enqueue(c, Run{RunID: runID, SessionID: sessionID}); err != nil {
			t.Fatal(err)
		}
		claimed, err := l.Claim(ctx, "w-st", time.Minute)
		if err != nil || claimed == nil || claimed.RunID != runID {
			t.Fatalf("claim %s = %+v err=%v", runID, claimed, err)
		}
		if err := l.Finish(ctx, runID, "w-st", "done"); err != nil {
			t.Fatal(err)
		}
	}

	// A tenant-tagged run makes the session resolve to that tenant; the
	// tenantless lookup crosses tenants because runs is NO FORCE RLS (the
	// queue-operator posture the wake driver shares).
	runTo("acme", "st-1", "s-acme")
	got, err := l.SessionTenant(ctx, "s-acme")
	if err != nil || got != "acme" {
		t.Fatalf("SessionTenant(s-acme) = %q, %v; want acme", got, err)
	}

	// The 'local' default and unknown sessions report no tenant.
	runTo("", "st-2", "s-plain")
	if got, err := l.SessionTenant(ctx, "s-plain"); err != nil || got != "" {
		t.Fatalf("SessionTenant(s-plain) = %q, %v; want \"\"", got, err)
	}
	if got, err := l.SessionTenant(ctx, "s-none"); err != nil || got != "" {
		t.Fatalf("SessionTenant(s-none) = %q, %v; want \"\"", got, err)
	}

	// Most-recent-wins: a newer run under another tenant re-scopes the
	// session (its updated_at is the freshest).
	runTo("other", "st-3", "s-acme")
	if got, err := l.SessionTenant(ctx, "s-acme"); err != nil || got != "other" {
		t.Fatalf("SessionTenant(s-acme) after newer run = %q, %v; want other", got, err)
	}
}
