package server

// Run-scope invariant tests (N2): every run the server starts must execute
// under a context tagged with the FINAL run/session identity
// (tracing.WithRun). Two paths historically violated it:
//
//   - goal runs (startGoalRun) never tagged their ctx at all, so tools and
//     spans inside a goal loop had no session correlation;
//   - session-less POST /api/runs tagged the ctx with the INPUT (empty)
//     session id before generating one, so the run executed with an empty
//     scope even though the response reported a fresh session id.
//
// The tests register a probe tool via agent.Config.ExtraTools that records
// tracing.RunFrom of its invocation ctx, script it into a FakeChatModel tool
// round (goal execute cycle / plain run first turn), and assert the recorded
// scope equals the identity the server reported for the run.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// runScopeProbe is a test-only tool that records the run scope its
// invocation ctx carries. It does nothing else; the scripted model calls it
// once so the HTTP-level test can observe what the server's run assembly
// put into the run context.
type runScopeProbe struct {
	mu      sync.Mutex
	scopes  []tracing.RunScope
	present []bool
}

func (p *runScopeProbe) Name() string              { return "RunScopeProbe" }
func (p *runScopeProbe) Description() string       { return "records the run scope from ctx" }
func (p *runScopeProbe) ArgsSchema() schema.Schema { return nil }

func (p *runScopeProbe) Invoke(ctx context.Context, _ map[string]any) (coretools.Result, error) {
	sc, ok := tracing.RunFrom(ctx)
	p.mu.Lock()
	p.scopes = append(p.scopes, sc)
	p.present = append(p.present, ok)
	p.mu.Unlock()
	return coretools.Result{Content: "ok"}, nil
}

// lastScope returns the most recently recorded scope and whether the ctx
// carried a run scope at all.
func (p *runScopeProbe) lastScope() (tracing.RunScope, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.scopes) == 0 {
		return tracing.RunScope{}, false
	}
	return p.scopes[len(p.scopes)-1], p.present[len(p.present)-1]
}

// count reports how many invocations were recorded.
func (p *runScopeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.scopes)
}

// newRunScopeTestServer builds a server whose agent exposes the probe tool
// and whose model plays the given scripted responses (ToolCalling enabled,
// permissions bypassed so the probe round never parks on an interrupt).
func newRunScopeTestServer(t *testing.T, probe *runScopeProbe, responses ...messages.Message) (*Server, *httptest.Server) {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(responses...),
		),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		ExtraTools:     []coretools.Tool{probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// drainSessionSaves waits for a run's async transcript save — it fires
// AFTER the done event, on a deliberately WithoutCancel ctx — and then for
// a quiet window, so t.TempDir cleanup never races a concurrent write
// (same pattern as TestGoalRunUnifiedReply and the SSE replay tests).
func drainSessionSaves(t *testing.T, saved <-chan string) {
	t.Helper()
	select {
	case <-saved:
	case <-time.After(10 * time.Second):
		t.Fatal("transcript save not completed")
	}
	for {
		select {
		case <-saved:
		case <-time.After(250 * time.Millisecond):
			return
		}
	}
}

// trackSaves installs the session-store save hook; call BEFORE the run
// starts so no save can slip past the channel.
func trackSaves(s *Server) chan string {
	saved := make(chan string, 8)
	s.sessions.testAfterSave = func(id string) { saved <- id }
	return saved
}

// TestGoalRunCarriesRunScope: the goal run's EXECUTE cycle must see the
// goal session's identity in its tool ctx — run id from the run, session id
// from the session the goal belongs to.
func TestGoalRunCarriesRunScope(t *testing.T) {
	probe := &runScopeProbe{}
	s, ts := newRunScopeTestServer(t, probe,
		messages.AI("criteria checklist\nThis cycle: probe"), // assess
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ // execute
			ID: "runscope-goal-1", Name: probe.Name(), Args: map[string]any{},
		}}},
		messages.AI("probed"), // execute final turn
		messages.AI(`{"verdict":"complete","evidence":"all green"}`), // evaluate
	)
	sid := "sess-goal-scope"
	saved := trackSaves(s)

	goal, runID, output := postGoal(t, ts.URL, sid, "set make the feature work")
	if goal == nil || goal.SessionID != sid || runID == "" {
		t.Fatalf("goal = %+v runID = %q output = %q", goal, runID, output)
	}
	streamUntilDone(t, ts.URL, runID)
	drainSessionSaves(t, saved)

	if probe.count() == 0 {
		t.Fatal("probe tool was never invoked; scripted execute cycle did not run")
	}
	sc, ok := probe.lastScope()
	if !ok || sc.RunID == "" || sc.SessionID == "" {
		t.Fatalf("goal run ctx carries no run scope (N2): probe saw %+v (present=%v); want run %q session %q",
			sc, ok, runID, sid)
	}
	if sc.SessionID != sid {
		t.Fatalf("goal run scope session = %q, want the goal session %q", sc.SessionID, sid)
	}
	if sc.RunID != runID {
		t.Fatalf("goal run scope run id = %q, want %q", sc.RunID, runID)
	}
}

// TestRunWithoutSessionIDScopesGeneratedSession: POST /api/runs without a
// session_id generates one; the run must execute scoped to THAT generated
// session id (the one returned in the response), not to the empty input id.
func TestRunWithoutSessionIDScopesGeneratedSession(t *testing.T) {
	probe := &runScopeProbe{}
	s, ts := newRunScopeTestServer(t, probe,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "runscope-plain-1", Name: probe.Name(), Args: map[string]any{},
		}}},
		messages.AI("done"),
	)
	saved := trackSaves(s)

	resp, err := http.Post(ts.URL+"/api/runs", "application/json",
		strings.NewReader(`{"prompt":"probe the run scope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/runs status = %d", resp.StatusCode)
	}
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" {
		t.Fatal("session-less POST /api/runs should generate and return a session id")
	}
	waitRunDone(t, ts.URL, created.RunID)
	drainSessionSaves(t, saved)

	if probe.count() == 0 {
		t.Fatal("probe tool was never invoked; scripted run did not execute")
	}
	sc, ok := probe.lastScope()
	if !ok || sc.RunID == "" || sc.SessionID == "" {
		t.Fatalf("run ctx carries an empty run scope (N2): probe saw %+v (present=%v); want run %q session %q",
			sc, ok, created.RunID, created.SessionID)
	}
	if sc.SessionID != created.SessionID {
		t.Fatalf("run scope session = %q, want the generated session %q", sc.SessionID, created.SessionID)
	}
	if sc.RunID != created.RunID {
		t.Fatalf("run scope run id = %q, want %q", sc.RunID, created.RunID)
	}
}
