package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newGoalTestServer builds a server whose agent can run one scripted goal
// cycle to a verified completion.
func newGoalTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("criteria checklist\nThis cycle: implement"), // assess
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
				ID: "1", Name: "Read", Args: map[string]any{"file_path": "x"},
			}}},
			messages.AI("implemented"),                                  // execute final
			messages.AI(`{"verdict":"complete","evidence":"all green"}`), // evaluate
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir, WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, dir
}

func postGoal(t *testing.T, base, sid, command string) (goal *agent.Goal, runID string, output string) {
	t.Helper()
	resp, err := http.Post(base+"/api/sessions/"+sid+"/goal", "application/json",
		strings.NewReader(fmt.Sprintf(`{"command":%q}`, command)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST goal %q: status %d", command, resp.StatusCode)
	}
	var got struct {
		Goal   *agent.Goal `json:"goal"`
		RunID  string      `json:"run_id"`
		Output string      `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got.Goal, got.RunID, got.Output
}

// streamUntilDone subscribes to a run's SSE stream and collects the event
// types seen until done/error.
func streamUntilDone(t *testing.T, base, runID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var types []string
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatalf("stream ended before done; saw %v", types)
		}
		types = append(types, ev.Type)
		if ev.Type == "done" {
			return types
		}
		if ev.Type == "error" {
			t.Fatalf("run error: %s", ev.Data)
		}
	}
}

func TestGoalAPIFullCycle(t *testing.T) {
	_, ts, dir := newGoalTestServer(t)
	sid := "sess-goal-1"

	goal, runID, output := postGoal(t, ts.URL, sid, "set make the feature work")
	if goal == nil || goal.Status != agent.GoalActive || goal.SessionID != sid {
		t.Fatalf("goal = %+v", goal)
	}
	if runID == "" {
		t.Fatal("set should start a goal run")
	}
	if !strings.Contains(output, "goal set") {
		t.Fatalf("output = %q", output)
	}

	types := streamUntilDone(t, ts.URL, runID)
	hasGoal, hasDelta := false, false
	for _, ty := range types {
		if ty == "goal" {
			hasGoal = true
		}
		if ty == "delta" {
			hasDelta = true
		}
	}
	if !hasGoal || !hasDelta {
		t.Fatalf("goal run events = %v (want goal + delta)", types)
	}

	// The goal reached a verified completion and persisted to the sidecar.
	g, _, _ := getGoal(t, ts.URL, sid)
	if g == nil || g.Status != agent.GoalComplete || g.Evidence != "all green" {
		t.Fatalf("final goal = %+v", g)
	}
	sidecar := filepath.Join(dir, sid+".goal.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("goal sidecar missing: %v", err)
	}

	// The session transcript persisted the cycle messages.
	resp, err := http.Get(ts.URL + "/api/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess struct {
		Messages []messages.Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	foundCycle, foundReport := false, false
	for _, m := range sess.Messages {
		if m.Role == messages.RoleHuman && strings.HasPrefix(m.Content, "<goal-cycle 1>") {
			foundCycle = true
		}
		if m.Role == messages.RoleAI && strings.Contains(m.Content, "Goal complete") {
			foundReport = true
		}
	}
	if !foundCycle || !foundReport {
		t.Fatalf("transcript missing cycle/report messages: %+v", sess.Messages)
	}
}

func getGoal(t *testing.T, base, sid string) (*agent.Goal, bool, error) {
	t.Helper()
	resp, err := http.Get(base + "/api/sessions/" + sid + "/goal")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Goal    *agent.Goal `json:"goal"`
		Running bool        `json:"running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got.Goal, got.Running, nil
}

func TestGoalAPIPauseResumeClear(t *testing.T) {
	_, ts, dir := newGoalTestServer(t)
	sid := "sess-goal-2"

	if _, _, output := postGoal(t, ts.URL, sid, "status"); !strings.Contains(output, "no goal set") {
		t.Fatalf("status without goal = %q", output)
	}
	if code, _ := postGoalErr(t, ts.URL, sid, "pause"); code != http.StatusNotFound {
		t.Fatal("pause without goal should 404")
	}

	// Budget-only goal: no run needed for this part.
	_, runID, _ := postGoal(t, ts.URL, sid, "set --cycles 9 finish everything")
	if runID == "" {
		t.Fatal("set should start a run")
	}
	// Wait for the scripted run to complete before mutating again.
	streamUntilDone(t, ts.URL, runID)

	if _, _, output := postGoal(t, ts.URL, sid, "budget tokens 5000000"); !strings.Contains(output, "tokens") {
		t.Fatalf("budget output = %q", output)
	}
	postGoal(t, ts.URL, sid, "clear")
	if g, _, _ := getGoal(t, ts.URL, sid); g != nil {
		t.Fatalf("goal after clear = %+v", g)
	}
	if _, err := os.Stat(filepath.Join(dir, sid+".goal.json")); !os.IsNotExist(err) {
		t.Fatal("clear should delete the goal sidecar")
	}
}

func postGoalErr(t *testing.T, base, sid, command string) (int, string) {
	t.Helper()
	resp, err := http.Post(base+"/api/sessions/"+sid+"/goal", "application/json",
		strings.NewReader(fmt.Sprintf(`{"command":%q}`, command)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	msg, _ := got["error"].(string)
	return resp.StatusCode, msg
}

func TestGoalAPIRestartRestoresFromSidecar(t *testing.T) {
	s1, ts1, dir := newGoalTestServer(t)
	sid := "sess-goal-3"
	_, runID, _ := postGoal(t, ts1.URL, sid, "set verify restarts")
	streamUntilDone(t, ts1.URL, runID)

	// A fresh server process over the same session dir sees the persisted
	// goal (post-restart display) and can restore it into a new agent.
	s2 := New(Config{Agent: s1.agent, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir, WorkDir: dir})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	g, _, _ := getGoal(t, ts2.URL, sid)
	if g == nil || g.Status != agent.GoalComplete || g.Objective != "verify restarts" {
		t.Fatalf("restored goal = %+v", g)
	}
}
