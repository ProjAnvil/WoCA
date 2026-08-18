package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"
)

// TestCreateSessionPersistsImmediately is the regression test for the
// vanishing workspace: creating a session must persist it (empty transcript
// plus index metadata) right away. The sidebar groups sessions by workspace,
// so an unpersisted session means a freshly added workspace disappears on the
// next page refresh (GET /api/sessions reads only the persisted index) —
// before this fix the session first materialized on disk when its initial
// run completed.
func TestCreateSessionPersistsImmediately(t *testing.T) {
	dir := t.TempDir()
	newServe := func(t *testing.T) *httptest.Server {
		t.Helper()
		s := New(Config{
			Agent:      newTestAgent(t),
			Static:     fstest.MapFS{},
			Logger:     zerolog.Nop(),
			SessionDir: dir,
			WorkDir:    dir,
		})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		return ts
	}

	ts := newServe(t)
	ws := filepath.Join(dir, "proj-x")
	resp, err := http.Post(ts.URL+"/api/sessions", "application/json",
		strings.NewReader(`{"workspace":`+quoteJSON(ws)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		SessionID     string `json:"session_id"`
		Workspace     string `json:"workspace"`
		WorkspaceName string `json:"workspace_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" || created.Workspace != ws || created.WorkspaceName != "proj-x" {
		t.Fatalf("create response = %+v", created)
	}

	// The session (and thus its workspace group) must be listed before any
	// run happens — this is what a page refresh replays on.
	listed := listWorkspaces(t, ts.URL)
	if _, ok := listed[created.SessionID]; !ok {
		t.Fatalf("fresh session not listed: %v", listed)
	}

	// And it must survive a server restart (the empty transcript must not be
	// pruned as a zombie index entry).
	ts2 := newServe(t)
	listed2 := listWorkspaces(t, ts2.URL)
	got, ok := listed2[created.SessionID]
	if !ok {
		t.Fatalf("session vanished across restart: %v", listed2)
	}
	if got != ws {
		t.Fatalf("workspace after restart = %q, want %q", got, ws)
	}
}

// TestCreateSessionWithoutWorkspacePersists covers the default-group case:
// no workspace in the request still persists under the server workdir so the
// auto-created first session does not multiply on every refresh.
func TestCreateSessionWithoutWorkspacePersists(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{
		Agent:      newTestAgent(t),
		Static:     fstest.MapFS{},
		Logger:     zerolog.Nop(),
		SessionDir: dir,
		WorkDir:    dir,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" {
		t.Fatal("no session_id in create response")
	}
	if _, ok := listWorkspaces(t, ts.URL)[created.SessionID]; !ok {
		t.Fatal("default-workspace session not listed after create")
	}
}

func listWorkspaces(t *testing.T, base string) map[string]string {
	t.Helper()
	resp, err := http.Get(base + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []struct {
		ID        string `json:"id"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range list {
		out[s.ID] = s.Workspace
	}
	return out
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
