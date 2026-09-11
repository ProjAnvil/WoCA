package tenantcmd

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func exec(t *testing.T, args ...string) error {
	t.Helper()
	cmd := New()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

func dbPath(t *testing.T) string {
	return "sqlite:" + filepath.Join(t.TempDir(), "t.db")
}

func TestTenantCmdLifecycle(t *testing.T) {
	db := dbPath(t)
	if err := exec(t, "--db", db, "create", "Acme"); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, "--db", db, "list"); err != nil {
		t.Fatal(err)
	}
	st, closeDB, err := OpenTenantStore(db)
	if err != nil {
		t.Fatal(err)
	}
	tenants, _ := st.ListTenants(t.Context())
	closeDB()
	if len(tenants) == 0 {
		t.Fatal("no tenants after create")
	}
	tid := tenants[0].ID
	if err := exec(t, "--db", db, "issue", tid, "--name", "ci-key"); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, "--db", db, "keys", tid); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, "--db", db, "revoke", "nonexistent"); err != nil {
		t.Fatal(err) // revoking unknown is not an error, but exec succeeds
	}
}

func TestTenantCmdRequiresDB(t *testing.T) {
	if err := exec(t, "--db", "none", "list"); err == nil {
		t.Fatal("tenant without a database succeeded")
	}
}

func TestTenantIssueUnknown(t *testing.T) {
	db := dbPath(t)
	if err := exec(t, "--db", db, "create", "X"); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, "--db", db, "issue", "t_missing"); err == nil {
		t.Fatal("issue for unknown tenant succeeded")
	}
}

var _ = cobra.Command{}
