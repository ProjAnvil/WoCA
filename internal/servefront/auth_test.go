package servefront

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/wocadb"
)

// TestServeAPIKeyAuth: `--auth apikey` end to end — the server authenticates
// against the tenant store on the settings DB; requests without a key get
// 401, a local-tenant viewer key gets read access, and its missing write
// scope is enforced. The default (local) mode is covered elsewhere.
func TestServeAPIKeyAuth(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	// Provision credentials on the same DB Serve will open.
	db, err := wocadb.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	_, viewerKey, err := st.IssueKey(context.Background(), "local", "u_local", "test", authz.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := st.CreateTenant(context.Background(), "remote-co")
	if err != nil {
		t.Fatal(err)
	}
	remoteUser, err := st.CreateUser(context.Background(), remote.ID, "rider", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, remoteKey, err := st.IssueKey(context.Background(), remote.ID, remoteUser.ID, "ci", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

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
	go func() { serveDone <- Serve(ctx, opts, addr, true, "apikey") }()

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
		if code := get("/api/sessions", viewerKey); code == http.StatusOK {
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
	if got := get("/api/sessions", viewerKey); got != http.StatusOK {
		t.Errorf("sessions with local viewer key = %d, want 200", got)
	}
	// Viewer scope: no runs:write → 403; check with a read of settings
	// (settings:read is admin-only for members/viewers).
	if got := get("/api/settings", viewerKey); got != http.StatusForbidden {
		t.Errorf("settings with viewer key = %d, want 403 (scope)", got)
	}
	// Remote tenant admin: default-deny on host surfaces.
	if got := get("/api/sessions", remoteKey); got != http.StatusForbidden {
		t.Errorf("sessions with remote tenant key = %d, want 403 (host surface)", got)
	}
	if got := get("/api/workspace/file?path=main.go", remoteKey); got != http.StatusForbidden {
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
