package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
)

// TestSessionContextEndpoint verifies GET /api/sessions/{id}/context: the
// hover-driven context indicator's payload — estimated categories from the
// session transcript, the model's context window from the provider table,
// and the compact threshold when configured.
func TestSessionContextEndpoint(t *testing.T) {
	s := New(Config{
		Agent:   newTestAgent(t),
		Static:  fstest.MapFS{},
		Logger:  zerolog.Nop(),
		WorkDir: t.TempDir(),
		CfgLive: func() config.Config {
			return config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5"}
		},
	})
	// A session with a small transcript (the workspace groups it in the index,
	// which is also where workspaceOf resolves the breakdown's workspace).
	sid := "ctx-test"
	if err := s.sessions.save(t.Context(), sid, []messages.Message{
		messages.Human("hello there"),
		messages.AI("hi!"),
	}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/sessions/" + sid + "/context")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var info agent.ContextInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Model != "claude-sonnet-4-5" {
		t.Fatalf("model = %q", info.Model)
	}
	if info.ContextWindow != 200_000 {
		t.Fatalf("context window = %d, want 200000", info.ContextWindow)
	}
	keys := map[string]bool{}
	for _, c := range info.Categories {
		keys[c.Key] = true
	}
	for _, k := range []string{"system", "memory", "tools", "history_human", "history_ai"} {
		if !keys[k] {
			t.Fatalf("category %q missing: %+v", k, info.Categories)
		}
	}
	if info.EstimatedTotal == 0 {
		t.Fatalf("estimated total is zero")
	}
}

// TestSessionContextUnknownSession verifies the endpoint answers for a
// session with no transcript (empty history, zero history categories) rather
// than erroring — hovering a freshly created, still-empty session must work.
func TestSessionContextUnknownSession(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/sessions/nope/context")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var info agent.ContextInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.EstimatedTotal == 0 {
		t.Fatalf("empty session should still count system prompt + tools")
	}
}
