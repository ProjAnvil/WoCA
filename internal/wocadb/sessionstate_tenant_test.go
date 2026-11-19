package wocadb

// Tenant-scoping of session_state writes (the FORCE RLS loop covers the
// table, so an unscoped write defaults to 'local' and the owning tenant's
// readers never see the busy row). The postgres GUC path is proven without
// a live server via the postgres-dialect-over-sqlite stand-in: the tenant
// transaction prologue fires (BeginTx + set_config) exactly as it would on
// a real postgres database and fails on the set_config call. The full RLS
// round trip lives behind WOCA_PG_TEST_DSN (see rls_pg_test.go).

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// newPgShapedDB opens a sqlite database driven as the postgres dialect.
func newPgShapedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open("sqlite:" + t.TempDir() + "/pgshaped.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &DB{DB: db.DB, Dialect: DialectPostgres, Path: db.Path}
}

func tenantCtx(id string) context.Context {
	return tenancy.NewContext(context.Background(), tenancy.Context{TenantID: id, UserID: "u1"})
}

// TestSessionStateWritesApplyTenantScope: with a non-local tenant in the
// context, every session_state write and read reaches the tenant
// transaction prologue — the set_config rejection is the observable (before
// this change the calls took no context at all and wrote straight to the
// pool, tagging rows 'local'). Without a tenant, or as the local tenant,
// the pool path stays.
func TestSessionStateWritesApplyTenantScope(t *testing.T) {
	db := newPgShapedDB(t)
	st := SessionState{SessionID: "s1", Status: SessionRunning, RunID: "r1", Owner: "1@h", UpdatedAt: Now()}

	if err := db.SetSessionState(tenantCtx("acme"), st); err == nil ||
		!strings.Contains(err.Error(), "session_state tenant tx scope") {
		t.Fatalf("SetSessionState with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}
	if _, err := db.ClearSessionState(tenantCtx("acme"), "s1", "r1"); err == nil ||
		!strings.Contains(err.Error(), "session_state tenant tx scope") {
		t.Fatalf("ClearSessionState with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}
	if err := db.DeleteSessionState(tenantCtx("acme"), "s1"); err == nil ||
		!strings.Contains(err.Error(), "session_state tenant tx scope") {
		t.Fatalf("DeleteSessionState with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}
	if _, err := db.SessionStates(tenantCtx("acme")); err == nil ||
		!strings.Contains(err.Error(), "session_state tenant tx scope") {
		t.Fatalf("SessionStates with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}

	// Controls: no tenant and the local tenant keep the pool path.
	if err := db.SetSessionState(context.Background(), st); err != nil {
		t.Fatalf("SetSessionState without tenant: %v", err)
	}
	if err := db.SetSessionState(tenantCtx("local"), st); err != nil {
		t.Fatalf("SetSessionState as local tenant: %v", err)
	}
	if _, err := db.SessionStates(context.Background()); err != nil {
		t.Fatalf("SessionStates without tenant: %v", err)
	}
}
