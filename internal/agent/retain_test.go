package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/settings"
)

func TestRetainRunStoresEpisodeMaterial(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{
		mem:     memoryRuntime{engine: f},
		workdir: "/ws",
		loadSettings: func() settings.Settings {
			s := settings.Default()
			s.LadyMAutoRetain = true
			s.LadyMSystem2 = false
			return s
		},
	}
	a.SetMemoryWorkspace("/ws/run")
	history := []messages.Message{messages.Human("fix the login bug")}
	final := []messages.Message{messages.System("sys"), messages.AI("fixed by rotating the JWT secret")}
	a.RetainRun(t.Context(), history, final, nil, nil)

	// The L2 fact layer has a single reviewed door (System 2 consolidation);
	// auto-retain writes the episode with the Q/A material only.
	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("auto-retain must not Remember: %v", calls)
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0], "woca/run/success") {
		t.Fatalf("events = %v", f.events)
	}
	obs := f.recordedEventObs()
	if len(obs) != 1 || !strings.Contains(obs[0], "Q: fix the login bug") ||
		!strings.Contains(obs[0], "A: fixed by rotating") {
		t.Fatalf("observation = %q", obs)
	}
}

func TestRetainRunDisabled(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMAutoRetain = false
	a := &Agent{
		mem:     memoryRuntime{engine: f, loadSettings: func() settings.Settings { return off }},
		workdir: "/ws",
	}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
}

func TestRetainRunEmptyAnswerKeepsQuestionOnly(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"} // loadSettings nil -> defaults (autoretain on)
	a.RetainRun(t.Context(), []messages.Message{messages.Human("q")}, nil, nil, nil)
	if obs := f.recordedEventObs(); len(obs) != 1 || obs[0] != "q" {
		t.Fatalf("observation = %v, want the bare question", obs)
	}
}

func TestRetainRunErrorOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, nil, errors.New("boom"), nil)
	if len(f.events) != 1 || !strings.Contains(f.events[0], "error") {
		t.Fatalf("events = %v", f.events)
	}
}

// A failed run that still produced a final text must not carry that answer
// into the episode: failed answers are history, not fact material.
func TestRetainRunErrorWithAnswerKeepsQuestionOnly(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")},
		[]messages.Message{messages.AI("half-baked answer")}, errors.New("boom"), nil)
	if obs := f.recordedEventObs(); len(obs) != 1 || obs[0] != "q" {
		t.Fatalf("observation = %v, want the bare question", obs)
	}
	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("failed run must not write facts: %v", calls)
	}
}

func TestRetainRunCancelledOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, nil, context.Canceled, nil)
	if len(f.events) != 1 || !strings.Contains(f.events[0], "cancelled") {
		t.Fatalf("events = %v", f.events)
	}
	if strings.Contains(f.events[0], "/error") {
		t.Fatalf("cancel must not map to error: %v", f.events)
	}
}

func TestRetainRunUsesLastQuestion(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	history := []messages.Message{
		messages.Human("first question"),
		messages.AI("first answer"),
		messages.Human("second question"),
		// Synthetic wrappers from earlier turns must be skipped.
		messages.Human("<relevant-memory>\nnoise\n</relevant-memory>"),
	}
	a.RetainRun(t.Context(), history, []messages.Message{messages.AI("second answer")}, nil, nil)
	obs := f.recordedEventObs()
	if len(obs) != 1 {
		t.Fatalf("observations = %v", obs)
	}
	if !strings.Contains(obs[0], "Q: second question") || strings.Contains(obs[0], "Q: first question") {
		t.Fatalf("observation = %q", obs[0])
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0], "success") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunLogsWriteErrors(t *testing.T) {
	f := &fakeEngine{eventErr: errors.New("disk full")}
	root := t.TempDir()
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws", obs: obslog.New(root, true)}
	// Wire the runtime's obs sink the way New() does for real agents.
	a.mem.log = a.obsLogSink()
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	data, err := os.ReadFile(filepath.Join(root, "woca.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "retain_write_error") || !strings.Contains(log, "disk full") {
		t.Fatalf("obs log = %q", log)
	}
}

func TestRetainRunSubagentNoop(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws", subagent: true}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("subagent must not retain (write choke point is the runner path)")
	}
}

func TestRetainRunNilEngineNoop(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil) // must not panic
}

func TestRetainGoalStoresEpisodeMaterial(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"} // loadSettings nil -> defaults (autoretain on)
	a.SetMemoryWorkspace("/ws/goal")
	a.RetainGoal(t.Context(), "ship the auth refactor", "complete", "tests pass: 3/3", nil)

	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("auto-retain must not Remember: %v", calls)
	}
	if len(f.events) != 1 || f.events[0] != "woca/goal:complete/success" {
		t.Fatalf("events = %v", f.events)
	}
	obs := f.recordedEventObs()
	if len(obs) != 1 || !strings.Contains(obs[0], "GOAL: ship the auth refactor") ||
		!strings.Contains(obs[0], "RESULT(complete): tests pass: 3/3") {
		t.Fatalf("observation = %q", obs)
	}
	if wss := f.recordedEventWSs(); len(wss) != 1 || wss[0] != "/ws/goal" {
		t.Fatalf("event workspaces = %v", wss)
	}
}

// A blocked goal's narrative is episode history, not fact material.
func TestRetainGoalBlockedOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(t.Context(), "migrate the db", "blocked", "needs credentials", nil)
	if len(f.events) != 1 || f.events[0] != "woca/goal:blocked/blocked" {
		t.Fatalf("events = %v", f.events)
	}
	if obs := f.recordedEventObs(); len(obs) != 1 || obs[0] != "migrate the db" {
		t.Fatalf("observation = %v, want the bare objective", obs)
	}
	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("blocked goal must not write facts: %v", calls)
	}
}

func TestRetainGoalDisabled(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMAutoRetain = false
	a := &Agent{
		mem:     memoryRuntime{engine: f, loadSettings: func() settings.Settings { return off }},
		workdir: "/ws",
	}
	a.RetainGoal(t.Context(), "objective", "complete", "summary", nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
}

func TestRetainGoalNilEngineNoop(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	a.RetainGoal(t.Context(), "objective", "complete", "summary", nil) // must not panic
}

func TestRetainGoalErrorOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(t.Context(), "objective", "complete", "summary", errors.New("boom"))
	if len(f.events) != 1 || f.events[0] != "woca/goal:complete/error" {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainGoalEmptySummaryKeepsObjectiveOnly(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(t.Context(), "objective", "budget_limited", "", nil)
	if obs := f.recordedEventObs(); len(obs) != 1 || obs[0] != "objective" {
		t.Fatalf("observation = %v, want the bare objective", obs)
	}
	if len(f.events) != 1 || f.events[0] != "woca/goal:budget_limited/budget_limited" {
		t.Fatalf("events = %v", f.events)
	}
}

func TestSystem2ThrottledPerWorkspace(t *testing.T) {
	f := &fakeEngine{}
	on := settings.Default()
	on.LadyMSystem2 = true
	a := &Agent{
		mem:     memoryRuntime{engine: f, loadSettings: func() settings.Settings { return on }},
		workdir: "/ws",
	}
	hist := []messages.Message{messages.Human("q")}
	fin := []messages.Message{messages.AI("a")}
	a.RetainRun(t.Context(), hist, fin, nil, nil)
	// fire-and-forget goroutine: poll for it to land
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.recordedConsolidates() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("consolidates = %d, want 1", got)
	}
	// Every cycle runs in mark mode (since = 0): the engine's
	// consolidated_at stamp decides what is pending.
	if since := f.recordedConsolidateSinces(); len(since) != 1 || since[0] != 0 {
		t.Fatalf("consolidate since args = %v, want [0]", since)
	}
	// second run within the interval: throttled
	a.RetainRun(t.Context(), hist, fin, nil, nil)
	time.Sleep(250 * time.Millisecond)
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("throttle failed: %d", got)
	}
	// different workspace: not throttled by the first
	a.SetMemoryWorkspace("/ws2")
	a.RetainRun(t.Context(), hist, fin, nil, nil)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.recordedConsolidates() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.recordedConsolidates(); got != 2 {
		t.Fatalf("per-workspace throttle wrong: %d", got)
	}
}

func TestSystem2DisabledNoConsolidate(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMSystem2 = false
	a := &Agent{
		mem:     memoryRuntime{engine: f, loadSettings: func() settings.Settings { return off }},
		workdir: "/ws",
	}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	time.Sleep(250 * time.Millisecond)
	if got := f.recordedConsolidates(); got != 0 {
		t.Fatalf("consolidates = %d, want 0", got)
	}
}

func TestSystem2RespectsIntervalSetting(t *testing.T) {
	f := &fakeEngine{}
	on := settings.Default()
	on.LadyMSystem2 = true
	on.LadyMSystem2Interval = 60 // minimum allowed
	a := &Agent{mem: memoryRuntime{engine: f, loadSettings: func() settings.Settings { return on }}, workdir: "/ws"}
	a.RetainRun(t.Context(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.recordedConsolidates() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("consolidates = %d, want 1", got)
	}
}

// system2Due throttles per workspace; the pending set itself is the engine's
// consolidated_at mark (see RetainRun's since=0), so only the interval is
// tested here.
func TestSystem2DueThrottle(t *testing.T) {
	m := memoryRuntime{}
	if !m.system2Due("w", 60) {
		t.Fatal("first check must be due")
	}
	if m.system2Due("w", 60) {
		t.Fatal("second check within the interval must be throttled")
	}
	if !m.system2Due("other", 60) {
		t.Fatal("different workspace must not be throttled by the first")
	}
}
