package edition

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// isolateEnv points HOME and the woca env overrides at a temp directory so
// tests never touch the developer's real ~/.woca state, and disables browser
// opening. Returns the temp home.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WOCA_DB", filepath.Join(home, "woca.db"))
	t.Setenv("WOCA_PLUGIN_ROOT", filepath.Join(home, "plugins"))
	t.Setenv("WOCA_NO_BROWSER", "1")
	return home
}

// seedSettings opens the isolated settings DB, applies key=value pairs, and
// saves.
func seedSettings(t *testing.T, pairs ...[2]string) {
	t.Helper()
	st, err := settings.Open(os.Getenv("WOCA_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	for _, p := range pairs {
		if err := settings.Apply(&cfg, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenAI serves one SSE chat completion round.
func fakeOpenAI(t *testing.T, content string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(delta map[string]any, finish any) {
			chunk := map[string]any{
				"id":     "chatcmpl-1",
				"object": "chat.completion.chunk",
				"choices": []map[string]any{{
					"index":         0,
					"delta":         delta,
					"finish_reason": finish,
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		writeChunk(map[string]any{"role": "assistant", "content": content}, nil)
		writeChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// baseOpts returns agentbuild.Options wired to an isolated environment with
// the OpenAI provider pointed at url and a temp workdir.
func baseOpts(t *testing.T, url string) agentbuild.Options {
	t.Helper()
	return agentbuild.Options{
		Provider: "openai",
		Model:    "gpt-test",
		APIKey:   "test-key",
		BaseURL:  url,
		Workdir:  t.TempDir(),
	}
}

// TestAuthnRequiresDB keeps the missing-database error observable.
func TestAuthnRequiresDB(t *testing.T) {
	if _, _, err := Authn(nil); err == nil {
		t.Fatal("nil settings store accepted")
	}
}

// TestServeAPIKeyAuthEndToEnd: `--auth apikey` through the real edition
// assembly — the server authenticates against the tenant store on the
// settings DB; requests without a key get 401, a local-tenant viewer key
// gets read access, its missing write scope is enforced, and a remote
// tenant is denied on host surfaces.
func TestServeAPIKeyAuthEndToEnd(t *testing.T) {
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
	_, viewerKey, err := st.IssueKey(t.Context(), "local", "u_local", "test", authz.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := st.CreateTenant(t.Context(), "remote-co")
	if err != nil {
		t.Fatal(err)
	}
	remoteUser, err := st.CreateUser(t.Context(), remote.ID, "rider", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, remoteKey, err := st.IssueKey(t.Context(), remote.ID, remoteUser.ID, "ci", authz.RoleAdmin)
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
	go func() {
		serveDone <- servefront.ServeWithEdition(ctx, opts, addr, true, "apikey", servefront.Edition{Authn: Authn})
	}()

	base := "http://" + addr
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

	// Wait for the real handler (health answers from port bind already).
	up := false
	for range 200 {
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
	// Viewer scope: settings:read is admin-only for members/viewers.
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
