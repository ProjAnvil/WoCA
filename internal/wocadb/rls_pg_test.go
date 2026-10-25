//go:build !norls

// Phase 1 RLS integration tests against a real PostgreSQL. Skipped unless
// WOCA_TEST_PG_DSN is set (e.g. the enterprise compose gateway:
// postgres://woca:…@127.0.0.1:35432/woca?sslmode=disable). Each run
// provisions and drops its own database, so the target instance is only
// used for CREATE/DROP DATABASE.
package wocadb_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/wocadb"
)

func testPGAdmin(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WOCA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("WOCA_TEST_PG_DSN not set; skipping postgres RLS integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	return db
}

// openIsolatedDB opens wocadb on a fresh database on the DSN's instance.
func openIsolatedDB(t *testing.T) *wocadb.DB {
	t.Helper()
	admin := testPGAdmin(t)
	defer admin.Close()
	name := fmt.Sprintf("woca_rls_test_%d", rand.Int63())
	role := name + "_app"
	pass := "rls-test"
	if _, err := admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
		t.Fatalf("drop stale db: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create db: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role)); err != nil {
		t.Fatalf("drop stale role: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS`, role, pass)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`GRANT ALL ON DATABASE %s TO %s`, name, role)); err != nil {
		t.Fatalf("grant db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
		_, _ = admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	})

	dsn := os.Getenv("WOCA_TEST_PG_DSN")

	// PG 15+ does not grant CREATE on a database's public schema to ordinary
	// roles; provision it via the admin connection before wocadb.Open runs
	// its migrations as the app role.
	grant, err := sql.Open("pgx", rewritePGDatabase(dsn, name))
	if err != nil {
		t.Fatalf("open grant conn: %v", err)
	}
	if _, err := grant.Exec(fmt.Sprintf(`GRANT CREATE ON SCHEMA public TO %s`, role)); err != nil {
		_ = grant.Close()
		t.Fatalf("grant schema create: %v", err)
	}
	_ = grant.Close()

	appDSN := rewritePGDatabase(dsn, name)
	appDSN = rewritePGUserPassword(appDSN, role, pass)
	db, err := wocadb.Open(appDSN)
	if err != nil {
		t.Fatalf("wocadb.Open as app role: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// rewritePGUserPassword swaps the user:password in a URL DSN.
func rewritePGUserPassword(dsn, user, pass string) string {
	i := indexByteFrom(dsn, ':') // scheme colon
	j := indexByteFrom(dsn[i+3:], '@')
	if j < 0 {
		return dsn
	}
	j += i + 3
	return dsn[:i+3] + user + ":" + pass + dsn[j:]
}

// rewritePGDatabase replaces the path component of a URL DSN.
func rewritePGDatabase(dsn, name string) string {
	// Only the URL form is used in tests; query params survive.
	i := len(dsn)
	if q := indexByteFrom(dsn, '?'); q >= 0 {
		i = q
	}
	slash := lastIndexByte(dsn[:i], '/')
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1] + name + dsn[i:]
}

func indexByteFrom(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// TestRLSMigrationProvisions: the postgres migration adds tenant_id, enables
// and forces RLS, and installs the tenant_isolation policy on every business
// table (FORCE matters: the app role owns these tables and would otherwise
// bypass row security entirely).
func TestRLSMigrationProvisions(t *testing.T) {
	db := openIsolatedDB(t)

	tables := []string{"settings", "secrets", "checkpoints", "memory", "session_state"}
	for _, table := range tables {
		var rlSecured, forced bool
		err := db.QueryRow(`
			SELECT c.relrowsecurity, c.relforcerowsecurity
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = $1 AND n.nspname = 'public'`, table).Scan(&rlSecured, &forced)
		if err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
		if !rlSecured || !forced {
			t.Errorf("table %s: rls=%v force=%v, want both true", table, rlSecured, forced)
		}
		var policies int
		if err := db.QueryRow(`SELECT count(*) FROM pg_policies
			WHERE tablename = $1 AND policyname = 'tenant_isolation'`, table).Scan(&policies); err != nil {
			t.Fatal(err)
		}
		if policies != 1 {
			t.Errorf("table %s: %d tenant_isolation policies, want 1", table, policies)
		}
		// tenant_id column exists.
		var hasCol bool
		if err := db.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'tenant_id')`, table).Scan(&hasCol); err != nil {
			t.Fatal(err)
		}
		if !hasCol {
			t.Errorf("table %s: tenant_id column missing", table)
		}
	}
}

// TestRLSLocalDefaultUnchanged: with no app.tenant_id set (every existing
// single-tenant deployment), rows written and read by the app keep working —
// they default to the 'local' tenant.
func TestRLSLocalDefaultUnchanged(t *testing.T) {
	db := openIsolatedDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('facts', 'k', '"v"', 1)`); err != nil {
		t.Fatalf("insert without tenant scope: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("local sees %d rows, want 1 (default tenant must behave as today)", n)
	}
}

// TestRLSTenantIsolation: the postgres-level two-tenants-invisible guarantee — transactions
// carrying SET LOCAL app.tenant_id see and write only their own rows; the
// local default tenant cannot see tenant rows and vice versa.
func TestRLSTenantIsolation(t *testing.T) {
	db := openIsolatedDB(t)
	ctx := context.Background()

	// A pre-existing local row.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('facts', 'local-row', '"v"', 1)`); err != nil {
		t.Fatal(err)
	}

	insertAs := func(tenant string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('facts', $1, '"x"', 1)`,
			tenant+"-row"); err != nil {
			t.Fatalf("insert as %s: %v", tenant, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	countAs := func(tenant string) int {
		t.Helper()
		var (
			tx  *sql.Tx
			n   int
			err error
		)
		if tenant == "" {
			err = db.QueryRowContext(ctx, `SELECT count(*) FROM memory`).Scan(&n)
		} else {
			tx, err = db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM memory`).Scan(&n)
		}
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	insertAs("t_aaa")
	insertAs("t_bbb")

	if got := countAs("t_aaa"); got != 1 {
		t.Errorf("tenant A sees %d rows, want 1 (its own only)", got)
	}
	if got := countAs("t_bbb"); got != 1 {
		t.Errorf("tenant B sees %d rows, want 1 (its own only)", got)
	}
	if got := countAs(""); got != 1 {
		t.Errorf("local default sees %d rows, want 1 (tenant rows must be invisible)", got)
	}

	// Deterministic version of the empty-string trap: on one pinned
	// connection that has carried a tenant transaction, the local default
	// view must still resolve to 'local' after the transaction ends (the
	// NULLIF guard in the policy; pooled connections otherwise re-read the
	// GUC as '' and match nothing).
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	txA, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txA.ExecContext(ctx, `SELECT set_config('app.tenant_id', 't_aaa', true)`); err != nil {
		t.Fatal(err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatal(err)
	}
	var nLocal int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM memory`).Scan(&nLocal); err != nil {
		t.Fatal(err)
	}
	if nLocal != 1 {
		t.Errorf("same-connection local view after tenant tx sees %d rows, want 1", nLocal)
	}

	// Row tagging: tenant transactions write rows tagged with their tenant.
	var tag string
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', 't_aaa', true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT tenant_id FROM memory WHERE key = 't_aaa-row'`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != "t_aaa" {
		t.Errorf("row written in tenant A tx tagged %q, want t_aaa (default from GUC)", tag)
	}
}

// TestStoreTenantIsolationPostgres: the memory store's tenant path on
// postgres — namespace prefixes plus SET LOCAL transactions — is invisible
// across tenants end to end.
func TestStoreTenantIsolationPostgres(t *testing.T) {
	db := openIsolatedDB(t)
	s := store.NewSQL(db.DB, string(db.Dialect), db.Path)
	ctx := context.Background()
	asA := tenancy.NewContext(ctx, tenancy.Context{TenantID: "t_aaa", UserID: "u"})
	asB := tenancy.NewContext(ctx, tenancy.Context{TenantID: "t_bbb", UserID: "u"})

	if err := s.Put(asA, "facts", "k", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(asB, "facts", "k", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "facts", "k", "local"); err != nil {
		t.Fatal(err)
	}

	get := func(ctx context.Context) string {
		v, ok, err := s.Get(ctx, "facts", "k")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return "<none>"
		}
		return string(v)
	}
	if got := get(asA); got != `"A"` {
		t.Errorf("A sees %s", got)
	}
	if got := get(asB); got != `"B"` {
		t.Errorf("B sees %s", got)
	}
	if got := get(ctx); got != `"local"` {
		t.Errorf("local sees %s", got)
	}

	for _, c := range []struct {
		ctx  context.Context
		want int
	}{{ctx, 1}, {asA, 1}, {asB, 1}} {
		entries, err := s.List(c.ctx, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != c.want {
			t.Errorf("list sees %d entries, want %d", len(entries), c.want)
		}
	}

	// Cross-tenant delete is a no-op (prefix) and cannot see the row (RLS).
	if err := s.Delete(asB, "facts", "k"); err != nil {
		t.Fatal(err)
	}
	if got := get(asA); got != `"A"` {
		t.Errorf("after B's delete, A sees %s — isolation broken", got)
	}
}
