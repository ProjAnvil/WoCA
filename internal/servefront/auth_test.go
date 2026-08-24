package servefront

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
)

// staticKeys is a test authn.Provider mapping tokens to tenancy contexts.
type staticKeys map[string]tenancy.Context

func (m staticKeys) Authenticate(_ context.Context, cred string) (tenancy.Context, error) {
	if tc, ok := m[cred]; ok {
		return tc, nil
	}
	return tenancy.Context{}, authn.ErrUnauthenticated
}

// TestServeAuthnEditionSeam: with an enterprise Edition attached, --auth
// apikey authenticates through the injected provider — no key is 401, a
// local viewer-scoped key reads but cannot read settings, and a remote
// tenant is denied on host surfaces. The provider here is a static fake;
// the tenant-store end-to-end lives in internal/ee/edition.
func TestServeAuthnEditionSeam(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	keys := staticKeys{
		"viewer-token": {TenantID: "local", UserID: "u_local", Scopes: []string{
			authz.ScopeSessionsRead, authz.ScopeRunsRead,
		}},
		"remote-token": {TenantID: "remote-co", UserID: "rider", Scopes: []string{
			authz.ScopeSessionsRead, authz.ScopeSessionsWrite,
			authz.ScopeRunsRead, authz.ScopeRunsWrite,
		}},
	}
	ed := Edition{Authn: func(*settings.Store) (authn.Provider, server.NativeAuth, error) {
		return keys, nil, nil
	}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- ServeWithEdition(ctx, opts, addr, true, "apikey", ed) }()

	base := "http://" + addr
	// get returns the status code, or -1 while the server is still starting
	// (connection refused during the agent build window).
	get := func(path, token string) int {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Wait for the real handler (health answers from port bind already);
	// /api/sessions is within the viewer scope.
	up := false
	for i := 0; i < 200; i++ {
		if code := get("/api/sessions", "viewer-token"); code == http.StatusOK {
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("server did not come up")
	}

	if got := get("/api/health", ""); got != http.StatusOK {
		t.Errorf("health without key = %d, want 200", got)
	}
	if got := get("/api/sessions", ""); got != http.StatusUnauthorized {
		t.Errorf("sessions without key = %d, want 401", got)
	}
	if got := get("/api/sessions", "bogus-token"); got != http.StatusUnauthorized {
		t.Errorf("sessions with unknown key = %d, want 401", got)
	}
	if got := get("/api/sessions", "viewer-token"); got != http.StatusOK {
		t.Errorf("sessions with local viewer key = %d, want 200", got)
	}
	// Viewer scope: no settings:read → 403.
	if got := get("/api/settings", "viewer-token"); got != http.StatusForbidden {
		t.Errorf("settings with viewer key = %d, want 403 (scope)", got)
	}
	// Remote tenant: default-deny on host surfaces.
	if got := get("/api/sessions", "remote-token"); got != http.StatusForbidden {
		t.Errorf("sessions with remote tenant key = %d, want 403 (host surface)", got)
	}
	if got := get("/api/workspace/file?path=main.go", "remote-token"); got != http.StatusForbidden {
		t.Errorf("workspace file with remote tenant key = %d, want 403", got)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil && fmt.Sprintf("%v", err) != "context canceled" {
			t.Fatalf("Serve returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not shut down")
	}
}
