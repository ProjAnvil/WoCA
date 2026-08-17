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

func TestRetainRunStoresFactAndEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{
		mem: memoryRuntime{engine: f},
		workdir:   "/ws",
		loadSettings: func() settings.Settings {
			s := settings.Default()
			s.LadyMAutoRetain = true
			return s
		},
	}
	a.SetMemoryWorkspace("/ws/run")
	history := []messages.Message{messages.Human("fix the login bug")}
	final := []messages.Message{messages.System("sys"), messages.AI("fixed by rotating the JWT secret")}
	a.RetainRun(context.Background(), history, final, nil)

	calls := f.recordedRememberCalls()
	if len(calls) != 1 {
		t.Fatalf("rememberCalls = %v", calls)
	}
	if !strings.Contains(calls[0], "woca:auto") || !strings.Contains(calls[0], "Q: fix the login bug") ||
		!strings.Contains(calls[0], "A: fixed by rotating") {
		t.Fatalf("content = %q", calls[0])
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0], "woca/run/success") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunDisabled(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMAutoRetain = false
	a := &Agent{
		mem:          memoryRuntime{engine: f, loadSettings: func() settings.Settings { return off }},
		workdir:      "/ws",
	}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
}

func TestRetainRunEmptyAnswerSkipsFactRecordsEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"} // loadSettings nil -> defaults (autoretain true)
	a.RetainRun(context.Background(), []messages.Message{messages.Human("q")}, nil, nil)
	if len(f.remembered) != 0 {
		t.Fatal("empty answer should not Remember")
	}
	if len(f.events) != 1 {
		t.Fatal("episode should still record")
	}
}

func TestRetainRunErrorOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, nil, errors.New("boom"))
	if len(f.events) != 1 || !strings.Contains(f.events[0], "error") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunCancelledOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, nil, context.Canceled)
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
	a.RetainRun(context.Background(), history, []messages.Message{messages.AI("second answer")}, nil)
	calls := f.recordedRememberCalls()
	if len(calls) != 1 {
		t.Fatalf("rememberCalls = %v", calls)
	}
	if !strings.Contains(calls[0], "Q: second question") || strings.Contains(calls[0], "Q: first question") {
		t.Fatalf("content = %q", calls[0])
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0], "success") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunLogsWriteErrors(t *testing.T) {
	f := &fakeEngine{rememberErr: errors.New("db locked"), eventErr: errors.New("disk full")}
	root := t.TempDir()
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws", obs: obslog.New(root, true)}
	// Wire the runtime's obs sink the way New() does for real agents.
	a.mem.log = a.obsLogSink()
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	data, err := os.ReadFile(filepath.Join(root, "woca.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "retain_write_error") || !strings.Contains(log, "db locked") ||
		!strings.Contains(log, "disk full") {
		t.Fatalf("obs log = %q", log)
	}
}

func TestRetainRunSubagentNoop(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws", subagent: true}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("subagent must not retain (write choke point is the runner path)")
	}
}

func TestRetainRunNilEngineNoop(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil) // must not panic
}

func TestRetainGoalStoresFactAndEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"} // loadSettings nil -> defaults (autoretain true)
	a.SetMemoryWorkspace("/ws/goal")
	a.RetainGoal(context.Background(), "ship the auth refactor", "complete", "tests pass: 3/3", nil)

	calls := f.recordedRememberCalls()
	if len(calls) != 1 {
		t.Fatalf("rememberCalls = %v", calls)
	}
	if !strings.Contains(calls[0], "/ws/goal") || !strings.Contains(calls[0], "woca:auto") ||
		!strings.Contains(calls[0], "GOAL: ship the auth refactor") ||
		!strings.Contains(calls[0], "RESULT(complete): tests pass: 3/3") {
		t.Fatalf("content = %q", calls[0])
	}
	if len(f.events) != 1 || f.events[0] != "woca/goal:complete/success" {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainGoalBlockedOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(context.Background(), "migrate the db", "blocked", "needs credentials", nil)
	if len(f.events) != 1 || f.events[0] != "woca/goal:blocked/blocked" {
		t.Fatalf("events = %v", f.events)
	}
	if calls := f.recordedRememberCalls(); len(calls) != 1 || !strings.Contains(calls[0], "RESULT(blocked): needs credentials") {
		t.Fatalf("rememberCalls = %v", calls)
	}
}

func TestRetainGoalDisabled(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMAutoRetain = false
	a := &Agent{
		mem:          memoryRuntime{engine: f, loadSettings: func() settings.Settings { return off }},
		workdir:      "/ws",
	}
	a.RetainGoal(context.Background(), "objective", "complete", "summary", nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
}

func TestRetainGoalNilEngineNoop(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	a.RetainGoal(context.Background(), "objective", "complete", "summary", nil) // must not panic
}

func TestRetainGoalErrorOutcome(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(context.Background(), "objective", "complete", "summary", errors.New("boom"))
	if len(f.events) != 1 || f.events[0] != "woca/goal:complete/error" {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainGoalEmptySummarySkipsFactRecordsEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.RetainGoal(context.Background(), "objective", "budget_limited", "", nil)
	if len(f.remembered) != 0 {
		t.Fatal("empty summary should not Remember")
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
		mem:          memoryRuntime{engine: f, loadSettings: func() settings.Settings { return on }},
		workdir:      "/ws",
	}
	hist := []messages.Message{messages.Human("q")}
	fin := []messages.Message{messages.AI("a")}
	a.RetainRun(context.Background(), hist, fin, nil)
	// fire-and-forget goroutine: poll for it to land
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.recordedConsolidates() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("consolidates = %d, want 1", got)
	}
	// second run within the interval: throttled
	a.RetainRun(context.Background(), hist, fin, nil)
	time.Sleep(250 * time.Millisecond)
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("throttle failed: %d", got)
	}
	// different workspace: not throttled by the first
	a.SetMemoryWorkspace("/ws2")
	a.RetainRun(context.Background(), hist, fin, nil)
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
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"} // loadSettings nil → defaults (system2 off)
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
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
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.recordedConsolidates() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.recordedConsolidates(); got != 1 {
		t.Fatalf("consolidates = %d, want 1", got)
	}
}
