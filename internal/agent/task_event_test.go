package agent

import (
	"context"
	"strings"
	"testing"
)

func TestRecordTaskEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.SetMemoryWorkspace("/ws/task")
	a.recordTaskEvent(t.Context(), "explorer", "map the auth flow", "done: 3 entry points found", nil)
	if len(f.events) != 1 {
		t.Fatalf("events = %v", f.events)
	}
	e := f.events[0]
	if !strings.Contains(e, "subagent:explorer") || !strings.Contains(e, "success") {
		t.Fatalf("event = %q", e)
	}
	a.recordTaskEvent(t.Context(), "explorer", "x", "", context.Canceled)
	if len(f.events) != 2 || !strings.Contains(f.events[1], "error") {
		t.Fatalf("error event missing: %v", f.events)
	}
}

func TestRecordTaskEventSubagentNoop(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws", subagent: true}
	a.recordTaskEvent(t.Context(), "x", "y", "z", nil)
	if len(f.events) != 0 {
		t.Fatal("subagent must not record task events")
	}
}
