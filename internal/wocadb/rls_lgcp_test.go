package wocadb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lgcppostgres "github.com/projanvil/langchain-golang/langgraph/checkpoint/postgres"
	"github.com/projanvil/woca/internal/cpserde"
)

// TestLgcpOwnershipPoliciesOnPostgres verifies the checkpoint tenant
// ownership RLS against a real postgres. Set WOCA_PG_TEST_DSN (any database
// on the server; the test creates and drops its own throwaway database):
//
//	WOCA_PG_TEST_DSN=postgres://woca:woca-enterprise-dev@127.0.0.1:35432/woca?sslmode=disable \
//	  go test ./internal/wocadb -run TestLgcpOwnership -v
func TestLgcpOwnershipPoliciesOnPostgres(t *testing.T) {
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()

	// Throwaway database: CREATE DATABASE cannot run in a transaction.
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_t7_%d", os.Getpid())
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatalf("drop stale db: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE `+dbName)
		_ = admin.Close()
	})

	// Point the DSN at the throwaway database.
	testDSN := baseDSN
	if i := indexOr(testDSN, "/woca?"); i >= 0 {
		testDSN = testDSN[:i] + "/" + dbName + testDSN[i+len("/woca?")-1:]
	} else {
		t.Fatalf("unsupported DSN shape for rewrite: %q", baseDSN)
	}
	db, err := Open("pg:" + testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// RLS never applies to the bootstrap superuser (POSTGRES_USER); real
	// deployments connect through a non-superuser application role. Run the
	// semantics as one.
	if _, err := admin.ExecContext(ctx, `DROP ROLE IF EXISTS woca_app_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE ROLE woca_app_test LOGIN PASSWORD 'probe' NOSUPERUSER`); err != nil {
		t.Fatalf("app role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS woca_app_test`)
	})
	grants := []string{
		`GRANT CONNECT ON DATABASE ` + dbName + ` TO woca_app_test`,
		`GRANT USAGE ON SCHEMA public, lgcp TO woca_app_test`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public, lgcp TO woca_app_test`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public, lgcp TO woca_app_test`,
		`GRANT EXECUTE ON FUNCTION lgcp.thread_owner(text) TO woca_app_test`,
	}
	for _, g := range grants {
		if _, err := db.DB.ExecContext(ctx, g); err != nil {
			t.Fatalf("grant %q: %v", g, err)
		}
	}
	app, err := sql.Open("pgx", strings.Replace(testDSN, "://woca:woca-enterprise-dev@", "://woca_app_test:probe@", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	// The gateway load-balances primary/replica; a brand-new role can take
	// a moment to replicate into the standby's auth catalog.
	ready := false
	for i := 0; i < 50 && !ready; i++ {
		if err := app.PingContext(ctx); err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("app role never became connectable (replication lag?)")
	}

	// The four upstream-shape tables exist in the lgcp schema with the
	// ownership policy attached.
	for _, table := range []string{"checkpoint_migrations", "checkpoints", "checkpoint_blobs", "checkpoint_writes"} {
		var n int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_tables WHERE schemaname='lgcp' AND tablename=`+"$1", table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("lgcp.%s missing (n=%d err=%v)", table, n, err)
		}
	}
	for _, table := range []string{"checkpoints", "checkpoint_blobs", "checkpoint_writes"} {
		var n int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_policies WHERE schemaname='lgcp' AND tablename=`+"$1"+` AND policyname='lgcp_tenant_isolation'`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("policy on lgcp.%s missing (n=%d err=%v)", table, n, err)
		}
	}

	// Ownership semantics. Two tenants and two threads: thread-owned-by-A
	// (a runs row for tenant A) and thread-unowned (no runs row).
	asTenant := func(tenant, thread string, write bool) error {
		tx, err := app.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', `+"$1"+`, true)`, tenant); err != nil {
			return err
		}
		const cp = `INSERT INTO lgcp.checkpoints (thread_id, checkpoint_ns, checkpoint_id, checkpoint) VALUES (`
		if write {
			if _, err := tx.ExecContext(ctx, cp+"$1"+`, '', 'c1', '{}'::jsonb)`, thread); err != nil {
				return err
			}
		} else {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM lgcp.checkpoints WHERE thread_id = `+"$1", thread).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("thread %s invisible to tenant %s", thread, tenant)
			}
		}
		return tx.Commit()
	}

	// Seed runs: tenant A owns thread-owned-by-A (written under tenant A's
	// GUC so the row carries tenant A, not the 'local' default).
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', 'tenantA', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs (run_id, session_id, status, owner, thread_id, created_at, updated_at)
		 VALUES ('r1','s1','done','w','thread-owned-by-A',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Unowned threads: any tenant writes and reads them (subagent/goal
	// scratch threads must never be locked out).
	if err := asTenant("tenantB", "thread-unowned", true); err != nil {
		t.Fatalf("unowned write as tenantB: %v", err)
	}
	if err := asTenant("tenantA", "thread-unowned", false); err != nil {
		t.Fatalf("unowned read as tenantA: %v", err)
	}

	// Owned threads: the owner writes and reads; a foreign tenant can
	// neither write nor see them.
	if err := asTenant("tenantA", "thread-owned-by-A", true); err != nil {
		t.Fatalf("owned write as tenantA: %v", err)
	}
	if err := asTenant("tenantA", "thread-owned-by-A", false); err != nil {
		t.Fatalf("owned read as tenantA: %v", err)
	}
	if err := asTenant("tenantB", "thread-owned-by-A", false); err == nil {
		t.Fatal("foreign tenant read an owned thread")
	}
	if err := asTenant("tenantB", "thread-owned-by-A", true); err == nil {
		t.Fatal("foreign tenant wrote an owned thread")
	}

	// End to end: the upstream postgres saver itself runs inside the lgcp
	// schema (search_path DSN, exactly how agentbuild configures it) and
	// its migrations no-op over the pre-created tables.
	saver, err := lgcppostgres.NewFromConnString(ctx, dsnWithSchema(testDSN), cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("saver: %v", err)
	}
	defer saver.Close()
	if err := saver.Setup(ctx); err != nil {
		t.Fatalf("Setup over pre-created schema: %v", err)
	}
	cfg := lgcheckpoint.Config{ThreadID: "thread-unowned"}
	cp := lgcheckpoint.Checkpoint{ID: "c1", ChannelValues: map[string]any{"k": int64(1)}}
	if _, err := saver.Put(ctx, cfg, cp, lgcheckpoint.Metadata{}, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	tup, err := saver.GetTuple(ctx, lgcheckpoint.Config{ThreadID: "thread-unowned"})
	if err != nil || tup == nil {
		t.Fatalf("GetTuple = %+v err=%v", tup, err)
	}
	if tup.Checkpoint.ID != "c1" {
		t.Fatalf("round trip id = %q", tup.Checkpoint.ID)
	}
}

func dsnWithSchema(dsn string) string {
	return dsn + "&search_path=lgcp"
}

func indexOr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
