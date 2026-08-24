// Phase 1 acceptance tests: tenant isolation on the API surface (design doc
// §13, Phase 1: 双租户互不可见 + /api/workspace/file 越权).
package tenant_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// newAuthTestServer builds a server.Server with the given authn provider, a real
// workspace file on disk, a settings store, and no agent (handlers degrade
// gracefully).
func newAuthTestServer(t *testing.T, provider authn.Provider) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := server.New(server.Config{
		Auth:       provider,
		Logger:     zerolog.Nop(),
		Static:     fstest.MapFS{},
		WorkDir:    dir,
		SessionDir: "",
		Settings:   st,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestUnauthenticatedRejected: with a strict provider, every /api route
// except the health probe demands a credential.
func TestUnauthenticatedRejected(t *testing.T) {
	strict := authn.NewStatic("tok", tenancy.Context{TenantID: "t1"})
	ts := newAuthTestServer(t, strict)

	if got := do(t, ts, http.MethodGet, "/api/health", ""); got != http.StatusOK {
		t.Fatalf("health = %d, want 200 (probe must stay open)", got)
	}
	for _, path := range []string{"/api/sessions", "/api/settings", "/api/workspace/file?path=hello.txt", "/api/runs/xyz/events"} {
		if got := do(t, ts, http.MethodGet, path, ""); got != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, got)
		}
	}
	if got := do(t, ts, http.MethodPost, "/api/runs", ""); got != http.StatusUnauthorized {
		t.Errorf("POST /api/runs unauthenticated = %d, want 401", got)
	}
}

// TestRemoteTenantDeniedHostSurfaces: the Phase 1 default-deny posture — a
// fully privileged non-local tenant is refused on every host surface (its
// runs would execute unsandboxed tools, its workspace reads would reach
// server-local paths, its settings reads would leak machine secrets). The
// health probe stays open. This is the API-level 双租户互不可见: tenant A
// can never observe anything belonging to the machine or another tenant.
func TestRemoteTenantDeniedHostSurfaces(t *testing.T) {
	tenantA := authn.NewStatic("key-a", tenancy.Context{
		TenantID: "t_aaa", UserID: "u1", Scopes: authz.AllScopes(),
	})
	ts := newAuthTestServer(t, tenantA)

	if got := do(t, ts, http.MethodGet, "/api/health", "key-a"); got != http.StatusOK {
		t.Fatalf("health = %d, want 200", got)
	}
	paths := []string{
		"/api/sessions",
		"/api/runs/xyz/events",
		"/api/events",
		"/api/workspace",
		"/api/workspace/file?path=hello.txt",
		"/api/workspace/file?path=../../../../etc/passwd",
		"/api/settings",
		"/api/memory",
		"/api/stats",
		"/api/completions?type=slash",
	}
	for _, p := range paths {
		if got := do(t, ts, http.MethodGet, p, "key-a"); got != http.StatusForbidden {
			t.Errorf("tenant A GET %s = %d, want 403", p, got)
		}
	}
	for _, p := range []string{"/api/runs", "/api/sessions", "/api/settings", "/api/memory"} {
		if got := do(t, ts, http.MethodPost, p, "key-a"); got != http.StatusForbidden {
			t.Errorf("tenant A POST %s = %d, want 403", p, got)
		}
	}
	// The SPA itself is not tenant data and keeps loading.
	if got := do(t, ts, http.MethodGet, "/", ""); got != http.StatusOK {
		t.Errorf("SPA / = %d, want 200", got)
	}
}

// TestWorkspaceFileUnauthorizedAccess: the Phase 1 acceptance case —
// /api/workspace/file must not serve host paths to any non-local caller
// (traversal, absolute, and plain relative variants all denied), while the
// local machine tenant keeps today's behavior.
func TestWorkspaceFileUnauthorizedAccess(t *testing.T) {
	tenantB := authn.NewStatic("key-b", tenancy.Context{
		TenantID: "t_bbb", UserID: "u1", Scopes: authz.AllScopes(),
	})
	ts := newAuthTestServer(t, tenantB)
	dir := t.TempDir()

	for _, q := range []string{
		"?path=hello.txt",
		"?path=" + filepath.Join(dir, "hello.txt"),
		"?path=../../etc/passwd",
		"?path=/etc/passwd",
	} {
		if got := do(t, ts, http.MethodGet, "/api/workspace/file"+q, "key-b"); got != http.StatusForbidden {
			t.Errorf("tenant B workspace file %q = %d, want 403", q, got)
		}
	}

	// Local machine tenant: unchanged behavior (still serves the file).
	localTS := newAuthTestServer(t, nil) // nil = authn.Local()
	if got := do(t, localTS, http.MethodGet, "/api/workspace/file?path=hello.txt", ""); got != http.StatusOK {
		t.Fatalf("local workspace file = %d, want 200", got)
	}
}

// TestScopeEnforcementLocalTenant: scopes bite for the local tenant too — a
// viewer-scoped local key can read sessions but not create runs or touch
// settings; the admin scope set can.
func TestScopeEnforcementLocalTenant(t *testing.T) {
	viewer := authn.NewStatic("v", tenancy.Context{
		TenantID: "local", UserID: "u", Scopes: authz.ScopesForRole(authz.RoleViewer),
	})
	admin := authn.NewStatic("a", tenancy.Context{
		TenantID: "local", UserID: "u", Scopes: authz.ScopesForRole(authz.RoleAdmin),
	})
	viewerTS := newAuthTestServer(t, viewer)
	adminTS := newAuthTestServer(t, admin)

	if got := do(t, viewerTS, http.MethodGet, "/api/sessions", "v"); got != http.StatusOK {
		t.Errorf("viewer GET sessions = %d, want 200", got)
	}
	if got := do(t, viewerTS, http.MethodPost, "/api/runs", "v"); got != http.StatusForbidden {
		t.Errorf("viewer POST runs = %d, want 403 (scope)", got)
	}
	if got := do(t, viewerTS, http.MethodGet, "/api/settings", "v"); got != http.StatusForbidden {
		t.Errorf("viewer GET settings = %d, want 403 (scope)", got)
	}
	if got := do(t, viewerTS, http.MethodDelete, "/api/memory/abc", "v"); got != http.StatusForbidden {
		t.Errorf("viewer DELETE memory = %d, want 403 (scope)", got)
	}
	if got := do(t, adminTS, http.MethodGet, "/api/settings", "a"); got != http.StatusOK {
		t.Errorf("admin GET settings = %d, want 200", got)
	}
}

// TestLocalModeUnchanged: the personal default (nil provider) keeps serving
// everything without credentials — the regression baseline for the
// single-binary form.
func TestLocalModeUnchanged(t *testing.T) {
	ts := newAuthTestServer(t, nil)
	for _, p := range []string{"/api/health", "/api/sessions", "/api/settings", "/api/workspace/file?path=hello.txt", "/api/memory", "/api/stats"} {
		if got := do(t, ts, http.MethodGet, p, ""); got != http.StatusOK {
			t.Errorf("local GET %s = %d, want 200", p, got)
		}
	}
}

// TestTenantAPIKeyProviderE2E: the shipped credential path — a key issued by
// the tenant store authenticates the server; a non-local tenant receives the
// default-deny posture, a "local" tenant key maps to the machine tenant
// with its granted scopes.
func TestTenantAPIKeyProviderE2E(t *testing.T) {
	ctx := context.Background()
	st := openTenantStoreForTest(t)

	ten, err := st.CreateTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, ten.ID, "alice", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, remoteKey, err := st.IssueKey(ctx, ten.ID, u.ID, "ci", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, localKey, err := st.IssueKey(ctx, "local", "u_local", "ops", authz.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}

	prov := tenant.NewAPIKeyProvider(st)
	apiTS := newAuthTestServer(t, prov)

	// Remote tenant: denied on host surfaces despite the admin role.
	if got := do(t, apiTS, http.MethodGet, "/api/workspace/file?path=hello.txt", remoteKey); got != http.StatusForbidden {
		t.Errorf("remote admin workspace file = %d, want 403", got)
	}
	// Local-tenant key: viewer scopes on the machine tenant.
	if got := do(t, apiTS, http.MethodGet, "/api/sessions", localKey); got != http.StatusOK {
		t.Errorf("local viewer sessions = %d, want 200", got)
	}
	if got := do(t, apiTS, http.MethodPost, "/api/runs", localKey); got != http.StatusForbidden {
		t.Errorf("local viewer runs = %d, want 403", got)
	}
	// Garbage credential: 401.
	if got := do(t, apiTS, http.MethodGet, "/api/sessions", "woca_deadbeefdead_deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); got != http.StatusUnauthorized {
		t.Errorf("garbage key = %d, want 401", got)
	}
}

// openTenantStoreForTest opens the tenant store on a fresh sqlite database
// (its migration seeds the 'local' machine tenant and u_local user).
func openTenantStoreForTest(t *testing.T) *tenant.Store {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
