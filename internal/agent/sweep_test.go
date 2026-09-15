package agent

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// Error-sweep: exercise error and edge branches across the Agent's
// non-graph surface (accessors, counters, memory store, task list) that
// the happy-path tests don't reach.
func TestAgentAccessorSweep(t *testing.T) {
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	// Accessors return zero values on a fresh agent.
	if a.ActiveTeammateCount() != 0 {
		t.Fatal("teammates on fresh agent")
	}
	if a.RunningTaskCount() != 0 {
		t.Fatal("tasks on fresh agent")
	}
	// LSPStats on a bare registry returns (0,0) or (0,N) — either is fine.
	_, _ = a.LSPStats()

	// Permission mode transitions.
	a.SetMode(config.ModeReadOnly)
	a.SetMode(config.ModeDefault)
	a.SetMode(config.ModeBypass)

	// Plan mode toggle.
	a.SetPlanMode(true)
	if !a.PlanMode() {
		t.Fatal("plan mode not set")
	}
	a.SetPlanMode(false)

	// Todos and task list round-trip.
	a.SetTodos([]tools.Todo{{Content: "todo 1"}, {Content: "todo 2"}})
	todos := a.Todos()
	if len(todos) != 2 {
		t.Fatalf("todos = %v", todos)
	}
	a.SetTodos(nil)

	// Memory engine is nil on this agent (not wired in Config).
	if eng := a.MemoryEngine(); eng != nil {
		t.Fatal("memory engine set without config")
	}

	// Context helpers.
	wsCtx := WithRunWorkspace(ctx, "/some/path")
	if wsCtx == nil {
		t.Fatal("WithRunWorkspace returned nil")
	}
	if _, err := a.Invoke(ctx, []messages.Message{messages.Human("hi")}, "sweep-thread"); err != nil {
		t.Fatal(err)
	}
	if got := a.LastThreadID(); got != "sweep-thread" {
		t.Fatalf("LastThreadID = %q", got)
	}

	// File history counts on a fresh agent.
	undo, redo := a.FileHistoryCounts(ctx)
	if undo != 0 || redo != 0 {
		t.Fatalf("file history = %d/%d", undo, redo)
	}

}
