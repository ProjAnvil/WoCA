package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/settings"
)

func TestRetainRunStoresFactAndEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{
		memEngine: f,
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
		memEngine:    f,
		workdir:      "/ws",
		loadSettings: func() settings.Settings { return off },
	}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
}

func TestRetainRunEmptyAnswerSkipsFactRecordsEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{memEngine: f, workdir: "/ws"} // loadSettings nil -> defaults (autoretain true)
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
	a := &Agent{memEngine: f, workdir: "/ws"}
	a.RetainRun(context.Background(),
		[]messages.Message{messages.Human("q")}, nil, context.Canceled)
	if len(f.events) != 1 || !strings.Contains(f.events[0], "error") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunSubagentNoop(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{memEngine: f, workdir: "/ws", subagent: true}
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
