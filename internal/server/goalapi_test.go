package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/obslog"
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
			messages.AI("implemented"),                                   // execute final
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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
	hasGoal, hasDelta := slices.Contains(types, "goal"), slices.Contains(types, "delta")
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

// streamGoalEventsUntilDone subscribes to a run's SSE stream and collects
// every event with its data payload until done/error.
func streamGoalEventsUntilDone(t *testing.T, base, runID string) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	out := map[string][]string{}
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatalf("stream ended before done; saw %v", out)
		}
		out[ev.Type] = append(out[ev.Type], ev.Data)
		if ev.Type == "done" {
			return out
		}
		if ev.Type == "error" {
			t.Fatalf("run error: %s", ev.Data)
		}
	}
}

func joinEventText(t *testing.T, events []string) string {
	t.Helper()
	var b strings.Builder
	for _, d := range events {
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(d), &p); err != nil {
			t.Fatalf("bad event payload %q: %v", d, err)
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// TestGoalRunUnifiedReply is the regression test for goal-loop chatter
// flooding the chat: intermediate cycle output (banners, the executor's
// per-cycle answers) must stream as goal_note events — never as chat deltas —
// and the run ends with ONE unified delta carrying the executor's final text
// plus the terminal report. The report message persists wrapped in
// <goal-report> markers so a transcript reload can tell it apart from
// intermediate cycle chatter.
func TestGoalRunUnifiedReply(t *testing.T) {
	s, ts, _ := newGoalTestServer(t)
	sid := "sess-goal-unified"
	// The transcript save fires after the final done event (its ctx is
	// deliberately WithoutCancel): wait for the saves to quiesce before
	// t.TempDir cleanup, same race as the SSE replay tests.
	saved := make(chan string, 8)
	s.sessions.testAfterSave = func(id string) { saved <- id }
	waitSaves := func() {
		t.Helper()
		select {
		case <-saved:
		case <-time.After(10 * time.Second):
			t.Fatal("transcript save not completed")
		}
		// A goal run may persist more than once: drain until a short
		// quiet window passes.
		for {
			select {
			case <-saved:
			case <-time.After(250 * time.Millisecond):
				return
			}
		}
	}

	_, runID, _ := postGoal(t, ts.URL, sid, "set make the feature work")
	events := streamGoalEventsUntilDone(t, ts.URL, runID)
	waitSaves()

	deltaText := joinEventText(t, events["delta"])
	noteText := joinEventText(t, events["goal_note"])

	if strings.Contains(deltaText, "[goal cycle") || strings.Contains(deltaText, "[goal verdict") {
		t.Fatalf("cycle banners leaked into chat deltas: %q", deltaText)
	}
	if !strings.Contains(noteText, "implemented") {
		t.Fatalf("executor cycle text should stream as goal_note events: %q", noteText)
	}
	if !strings.Contains(deltaText, "implemented") || !strings.Contains(deltaText, "Goal complete") {
		t.Fatalf("unified reply should carry the executor text and the report, got: %q", deltaText)
	}

	// The persisted report message is wrapped so reload rendering can
	// distinguish it from intermediate cycle chatter.
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
	foundWrapped := false
	for _, m := range sess.Messages {
		if m.Role == messages.RoleAI && strings.HasPrefix(m.Content, "<goal-report>") {
			foundWrapped = true
			if !strings.Contains(m.Content, "Goal complete") {
				t.Fatalf("wrapped report = %q", m.Content)
			}
		}
	}
	if !foundWrapped {
		t.Fatalf("no <goal-report>-wrapped AI message in transcript: %+v", sess.Messages)
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

// newGoalLifecycleServer builds a server whose goal run parks at an
// AskUserQuestion interrupt inside the first execute cycle, with
// observability wired (the goal-run start logs through it).
func newGoalLifecycleServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("criteria checklist"),
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "AskUserQuestion", Args: map[string]any{
					"questions": []any{map[string]any{"question": "pick one"}},
				}},
			}},
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		Obs:            obslog.New(t.TempDir(), false),
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

// cancelParkedGoalRun cancels a goal run parked at an interrupt and reads the
// SAME stream to its terminal event, asserting a cancelled (not failed) done.
func cancelParkedGoalRun(t *testing.T, base, runID string, sc *bufio.Scanner) {
	t.Helper()
	if err := postJSON(t, base+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		if ev.Type == "error" {
			t.Fatalf("cancelled goal run errored: %s", ev.Data)
		}
		if ev.Type == "done" {
			if !strings.Contains(ev.Data, `"cancelled":true`) {
				t.Fatalf("done payload = %s, want cancelled:true", ev.Data)
			}
			return
		}
	}
}

func TestGoalCommandLifecycleWhileRunning(t *testing.T) {
	s, ts, _ := newGoalLifecycleServer(t)
	sid := "sess-goal-life"

	// Malformed command bodies are 400s.
	badResp, err := http.Post(ts.URL+"/api/sessions/"+sid+"/goal", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", badResp.StatusCode)
	}

	// help is a pure documentation reply.
	if _, _, output := postGoal(t, ts.URL, sid, "help"); !strings.Contains(output, "goal mode") {
		t.Fatalf("help output = %q", output)
	}

	goal, runID, _ := postGoal(t, ts.URL, sid, "keep working")
	if goal == nil || goal.SessionID != sid {
		t.Fatalf("goal = %+v", goal)
	}

	// Wait until the loop parks at its interrupt.
	resp, sc := waitForRunInterrupt(t, ts.URL, runID)
	defer resp.Body.Close()

	// The internal starter refuses a second concurrent loop.
	if _, err := s.startGoalRun(sid, ""); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("startGoalRun while running = %v, want already-running error", err)
	}

	// While the loop is running: pause is honored…
	if _, _, output := postGoal(t, ts.URL, sid, "pause"); !strings.Contains(output, "goal paused") {
		t.Fatalf("pause output = %q", output)
	}
	// …set conflicts…
	if code, msg := postGoalErr(t, ts.URL, sid, "set another objective"); code != http.StatusConflict {
		t.Fatalf("set while running: status %d (%s), want 409", code, msg)
	}
	// …resume is a no-op note…
	if _, _, output := postGoal(t, ts.URL, sid, "resume"); !strings.Contains(output, "already running") {
		t.Fatalf("resume while running output = %q", output)
	}
	// …and budget adjustments still work.
	if _, _, output := postGoal(t, ts.URL, sid, "budget cycles 7"); !strings.Contains(output, "cycle") {
		t.Fatalf("budget cycles output = %q", output)
	}

	// Cancel the parked loop: it must end as cancelled, not failed.
	cancelParkedGoalRun(t, ts.URL, runID, sc)

	// A paused goal resumes into a fresh loop.
	_, runID2, output := postGoal(t, ts.URL, sid, "resume")
	if runID2 == "" || !strings.Contains(output, "resumed") {
		t.Fatalf("resume output = %q runID = %q", output, runID2)
	}
	resp2, sc2 := waitForRunInterrupt(t, ts.URL, runID2)
	defer resp2.Body.Close()
	cancelParkedGoalRun(t, ts.URL, runID2, sc2)

	// Setting a new objective replaces the old one (clear + set + new loop).
	goal3, runID3, _ := postGoal(t, ts.URL, sid, "set a brand new objective")
	if goal3 == nil || goal3.Objective != "a brand new objective" || runID3 == "" {
		t.Fatalf("re-set goal = %+v runID %q", goal3, runID3)
	}
	resp3, sc3 := waitForRunInterrupt(t, ts.URL, runID3)
	defer resp3.Body.Close()
	cancelParkedGoalRun(t, ts.URL, runID3, sc3)
}

func TestGoalResumeAfterRestart(t *testing.T) {
	_, ts1, dir := newGoalTestServer(t)
	sid := "sess-goal-restart"
	_, runID, _ := postGoal(t, ts1.URL, sid, "finish the migration")
	streamUntilDone(t, ts1.URL, runID)

	// A fresh agent over the same session dir: the goal lives only in the
	// sidecar, so resume restores it (complete → "already complete").
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("ok")),
	)
	a2, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(Config{Agent: a2, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir, WorkDir: dir})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()

	if _, runID, output := postGoal(t, ts2.URL, sid, "resume"); runID != "" || !strings.Contains(output, "already complete") {
		t.Fatalf("resume after restart = runID %q output %q", runID, output)
	}
	// A complete goal cannot pause (409)…
	if code, _ := postGoalErr(t, ts2.URL, sid, "pause"); code != http.StatusConflict {
		t.Fatalf("pause complete goal: status %d, want 409", code)
	}
	// …but its budget is adjustable…
	if _, _, output := postGoal(t, ts2.URL, sid, "budget cycles 3"); !strings.Contains(output, "cycle") {
		t.Fatalf("budget on restored goal = %q", output)
	}
	// …and the loop starter refuses non-active goals.
	if _, err := s2.startGoalRun(sid, ""); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("startGoalRun on complete goal = %v, want not-active error", err)
	}

	// Clear (live goal), clear again (nothing left), and the lonely verbs.
	if _, _, output := postGoal(t, ts2.URL, sid, "clear"); !strings.Contains(output, "goal cleared") {
		t.Fatalf("clear output = %q", output)
	}
	if _, _, output := postGoal(t, ts2.URL, sid, "clear"); !strings.Contains(output, "no goal set") {
		t.Fatalf("second clear output = %q", output)
	}
	if _, runID, output := postGoal(t, ts2.URL, sid, "resume"); runID != "" || !strings.Contains(output, "no goal set") {
		t.Fatalf("resume without goal = runID %q output %q", runID, output)
	}
	if code, _ := postGoalErr(t, ts2.URL, sid, "budget cycles 3"); code != http.StatusNotFound {
		t.Fatalf("budget without goal: status %d, want 404", code)
	}
}

func TestGoalRunModelError(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithRateLimiter(errorLimiter{err: errors.New("model offline")}),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	_, runID, _ := postGoal(t, ts.URL, "sess-goal-err", "set do the thing")
	if runID == "" {
		t.Fatal("set should start a run")
	}
	typ, data := streamTerminalEvent(t, ts.URL, runID)
	if typ != "error" {
		t.Fatalf("goal run terminal = %s %s, want error", typ, data)
	}
}

// TestGoalSubscriberWithoutOwner: goal notifications for a goal with no
// session owner (the TUI path) are dropped by the mirroring subscriber
// instead of writing a sidecar for an empty id.
func TestGoalSubscriberWithoutOwner(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir})

	if _, err := s.agent.SetGoal("tui objective", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if g := s.agent.Goal(); g == nil || g.Objective != "tui objective" {
		t.Fatalf("goal = %+v", g)
	}
	// No sidecar appears for the empty owner…
	if _, err := os.Stat(filepath.Join(dir, ".goal.json")); !os.IsNotExist(err) {
		t.Fatalf("unowned goal wrote a sidecar: %v", err)
	}
	// …and clearing it stays a no-op.
	if !s.agent.ClearGoal() {
		t.Fatal("clear should report the removed goal")
	}
}
