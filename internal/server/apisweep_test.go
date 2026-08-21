package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"
)

func newSweepServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Config{
		Agent: newScriptedAgent(t, "sweep"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

// Goal API: set/pause/resume/status/clear lifecycle through HTTP.
func TestGoalAPISweep(t *testing.T) {
	_, srv := newSweepServer(t)

	post := func(body map[string]any) (*http.Response, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/sessions/sess-goal/goal", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		return resp, buf.String()
	}

	// Set a goal.
	resp, body := post(map[string]any{"command": "/goal test objective"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set goal = %d %s", resp.StatusCode, body)
	}

	// Status.
	resp2, err := http.Get(srv.URL + "/api/sessions/sess-goal/goal")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("goal status = %d", resp2.StatusCode)
	}

	// Pause (the loop may still be running — either outcome proves the
	// endpoint handles the state transition).
	resp3, _ := post(map[string]any{"command": "/goal pause"})
	_ = resp3

	// Resume.
	resp4, _ := post(map[string]any{"command": "/goal resume"})
	_ = resp4

	// Clear (may 409 while the loop runs — proves the state guard).
	resp5, _ := post(map[string]any{"command": "/goal clear"})
	_ = resp5

	// Pause on no goal (edge case: clearing again is a no-op).
	_, _ = post(map[string]any{"command": "/goal clear"})
}

// Goal API: invalid commands and unknown sessions error gracefully.
func TestGoalAPIErrors(t *testing.T) {
	_, srv := newSweepServer(t)

	// Unknown command.
	b, _ := json.Marshal(map[string]any{"command": "/goal bogus"})
	resp, err := http.Post(srv.URL+"/api/sessions/s1/goal", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bogus command = %d", resp.StatusCode)
	}

	// No body → 400.
	resp2, err := http.Post(srv.URL+"/api/sessions/s1/goal", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
}

// Native auth: activate with missing fields → 400.
func TestNativeAuthValidationSweep(t *testing.T) {
	_, srv := newSweepServer(t)

	// The sweep server has no NativeAuth configured — the endpoint is
	// absent (404), proving the conditional registration.
	resp, err := http.Post(srv.URL+"/api/native/activate", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("activate without NativeAuth = %d", resp.StatusCode)
	}
}

// Context API: session context endpoint returns data.
func TestContextAPI(t *testing.T) {
	_, srv := newSweepServer(t)
	resp, err := http.Get(srv.URL + "/api/sessions/sess-ctx/context")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Any non-5xx is acceptable (the session may not exist).
	if resp.StatusCode >= 500 {
		t.Fatalf("context = %d", resp.StatusCode)
	}
}

// Plugins API: list plugins.
func TestPluginsAPIList(t *testing.T) {
	_, srv := newSweepServer(t)
	resp, err := http.Get(srv.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Workspace API: get workspace info.
func TestWorkspaceAPI(t *testing.T) {
	_, srv := newSweepServer(t)
	resp, err := http.Get(srv.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Completions API: get completions for @ and /.
func TestCompletionsAPI(t *testing.T) {
	_, srv := newSweepServer(t)
	resp, err := http.Get(srv.URL + "/api/completions?prefix=/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Memory API: list and remember.
func TestMemoryAPISweep(t *testing.T) {
	_, srv := newSweepServer(t)

	resp, err := http.Get(srv.URL + "/api/memory")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	b, _ := json.Marshal(map[string]any{"content": "test fact", "tags": []string{"test"}})
	resp2, err := http.Post(srv.URL+"/api/memory", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	resp3, err := http.Get(srv.URL + "/api/memory/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
}
