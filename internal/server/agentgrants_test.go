package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// A test server whose agent carries the production per-session policy
// store (agentbuild's wiring), so grant isolation is observable.
func newGrantsServer(t *testing.T) (*Server, string) {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(messages.AI("ok")),
		),
		Registry:       tools.NewRegistry(t.TempDir()),
		PermissionMode: config.ModeDefault,
		PolicySource:   agent.NewPerSessionPolicySource(config.ModeDefault),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

// Task 11: the session-grants surface backing the web grants panel —
// GET lists a session's granted rule keys, DELETE revokes one, and both
// route through the agent's SessionPolicySource mutators (§3.3).
func TestAgentGrantsListAndRevoke(t *testing.T) {
	s, base := newGrantsServer(t)

	// Seed two grants on the session's policy store (the approval flow's
	// "allow for session" write path).
	src := s.agent.PolicySource()
	src.AddGrant("session-1", "bash:git push*")
	src.AddGrant("session-1", "domain:example.com")
	src.AddGrant("session-2", "websearch:latest AI*")

	// GET lists the session's grants only, sorted.
	resp, err := http.Get(base + "/api/agent/grants?session=" + url.QueryEscape("session-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Session string `json:"session"`
		Grants  []struct {
			Key string `json:"key"`
		} `json:"grants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Session != "session-1" || len(out.Grants) != 2 {
		t.Fatalf("unexpected listing: %+v", out)
	}
	if out.Grants[0].Key != "bash:git push*" || out.Grants[1].Key != "domain:example.com" {
		t.Fatalf("grants not sorted or wrong: %+v", out.Grants)
	}

	// DELETE revokes one key; the list shrinks, other sessions keep theirs.
	req, err := http.NewRequest(http.MethodDelete,
		base+"/api/agent/grants?session="+url.QueryEscape("session-1")+"&key="+url.QueryEscape("bash:git push*"), nil)
	if err != nil {
		t.Fatal(err)
	}
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", dresp.StatusCode)
	}
	if got := src.ListGrants("session-1"); len(got) != 1 || got[0] != "domain:example.com" {
		t.Fatalf("session-1 grants after revoke: %v", got)
	}
	if got := src.ListGrants("session-2"); len(got) != 1 {
		t.Fatalf("session-2 grants must be untouched: %v", got)
	}

	// A missing key is a client error; revoking an absent key is a no-op.
	req, _ = http.NewRequest(http.MethodDelete, base+"/api/agent/grants", nil)
	bresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bresp.Body.Close()
	if bresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("keyless delete status %d, want 400", bresp.StatusCode)
	}
}
