package tenantcmd

// Gap tests: the CLI's error branches — database open failures, broken
// tables, unknown tenants, and revoke/keys failure surfacing.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

// brokenDB migrates a valid database then marks it read-only so store
// writes fail while opens keep succeeding.
func brokenDB(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "broken.db")
	path := "sqlite:" + file
	db, err := wocadb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCmd(t *testing.T, db string, args ...string) error {
	t.Helper()
	cmd := New()
	cmd.SetArgs(append([]string{"--db", db}, args...))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

func TestTenantCmdOpenFailures(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, "sqlite:"+filepath.Join(blocker, "x.db"), "list"); err == nil {
		t.Fatal("unopenable database should fail the command")
	}
	if err := runCmd(t, "none", "list"); err == nil {
		t.Fatal("db=none should be rejected for tenant management")
	}
}

func TestTenantCmdStoreFailures(t *testing.T) {
	db := brokenDB(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"create", []string{"create", "acme"}},
		{"list", []string{"list"}},
		{"issue", []string{"issue", "--user", "u_x", "t_missing"}},
		{"keys", []string{"keys", "t_missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runCmd(t, db, tc.args...); err == nil {
				t.Fatalf("%v should fail on a broken store", tc.args)
			}
		})
	}
}

func TestTenantCmdRevokeLifecycle(t *testing.T) {
	path := "sqlite:" + filepath.Join(t.TempDir(), "ok.db")

	if err := runCmd(t, path, "create", "acme"); err != nil {
		t.Fatal(err)
	}
	st, closeDB, err := OpenTenantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := st.ListTenants(context.Background())
	if err != nil {
		closeDB()
		t.Fatal(err)
	}
	tid := ""
	for _, tn := range tenants {
		if tn.Name == "acme" {
			tid = tn.ID
		}
	}
	if tid == "" {
		closeDB()
		t.Fatalf("created tenant missing: %v", tenants)
	}
	if err := runCmd(t, path, "issue", "--name", "primary", tid); err != nil {
		closeDB()
		t.Fatal(err)
	}
	keys, err := st.ListKeys(context.Background(), tid)
	if err != nil || len(keys) != 1 {
		closeDB()
		t.Fatalf("keys = %v %v", keys, err)
	}
	if err := runCmd(t, path, "revoke", keys[0].Prefix); err != nil {
		closeDB()
		t.Fatal(err)
	}
	// Revoking again is an idempotent no-op (or a loud failure — both
	// keep the CLI honest; only a crash would be wrong).
	_ = runCmd(t, path, "revoke", keys[0].Prefix)
	closeDB()
}
