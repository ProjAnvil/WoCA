package main

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/ee/tenantcmd"
)

func tenantDB(t *testing.T) string {
	t.Helper()
	return "sqlite:" + filepath.Join(t.TempDir(), "tenant.db")
}

func TestTenantCmdLifecycle(t *testing.T) {
	isolateEnv(t)
	db := tenantDB(t)

	if err := execCmd(t, "tenant", "create", "Acme", "--db", db); err != nil {
		t.Fatal(err)
	}
	// Second create with the same name still succeeds (distinct ids).
	if err := execCmd(t, "tenant", "create", "Acme", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "tenant", "list", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "tenant", "create-user", "t_0", "alice", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "tenant", "issue", "t_missing", "--db", db); err == nil {
		t.Fatal("issue for unknown tenant succeeded")
	}
}

func TestTenantIssueAndListKeys(t *testing.T) {
	isolateEnv(t)
	db := tenantDB(t)
	if err := execCmd(t, "tenant", "create", "Beta", "--db", db); err != nil {
		t.Fatal(err)
	}
	// Resolve the created tenant id from the shared store (the CLI prints it
	// to stdout; going through the store keeps the test hermetic).
	st, closeDB, err := tenantcmd.OpenTenantStore(db)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := st.ListTenants(nil)
	if err != nil || len(tenants) == 0 {
		closeDB()
		t.Fatalf("tenants = %v err=%v", tenants, err)
	}
	tid := tenants[0].ID
	closeDB()
	if err := execCmd(t, "tenant", "create-user", tid, "bob", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "tenant", "issue", tid, "--user", "u_missing", "--db", db); err == nil {
		t.Fatal("issue for unknown user succeeded")
	}
	// Default path: no --user creates a named user, then issues the key.
	if err := execCmd(t, "tenant", "issue", tid, "--name", "ci-key", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "tenant", "keys", tid, "--db", db); err != nil {
		t.Fatal(err)
	}
}

func TestTenantCmdRequiresDB(t *testing.T) {
	isolateEnv(t)
	if err := execCmd(t, "tenant", "list", "--db", "none"); err == nil {
		t.Fatal("tenant command without a database succeeded")
	}
}
