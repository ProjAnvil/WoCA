package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestMemoryWorkspaceCtxTiering(t *testing.T) {
	// Bottom tier: workdir on a bare agent.
	a := &Agent{workdir: "/ws/workdir"}
	if got := a.memoryWorkspaceCtx(t.Context()); got != "/ws/workdir" {
		t.Fatalf("workdir tier = %q, want /ws/workdir", got)
	}
	// Middle tier: the agent field (TUI startup pin / fork inheritance).
	a.SetMemoryWorkspace("/ws/field")
	if got := a.memoryWorkspaceCtx(t.Context()); got != "/ws/field" {
		t.Fatalf("field tier = %q, want /ws/field", got)
	}
	// Top tier: the ctx value beats the field.
	ctx := WithRunWorkspace(t.Context(), "/ws/ctx")
	if got := a.memoryWorkspaceCtx(ctx); got != "/ws/ctx" {
		t.Fatalf("ctx tier = %q, want /ws/ctx", got)
	}
	// A bare agent (no workdir, no field) still resolves the ctx value.
	if got := (&Agent{}).memoryWorkspaceCtx(ctx); got != "/ws/ctx" {
		t.Fatalf("bare agent ctx tier = %q, want /ws/ctx", got)
	}
	// Values survive cancellation (the runner defer reads them post-cancel).
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := a.memoryWorkspaceCtx(cancelled); got != "/ws/ctx" {
		t.Fatalf("cancelled ctx tier = %q, want /ws/ctx", got)
	}
}

func TestWithRunWorkspaceEmptyNoop(t *testing.T) {
	ctx := context.Background()
	if out := WithRunWorkspace(ctx, ""); out != ctx {
		t.Fatal("empty workspace must return the ctx unchanged")
	}
	if ws, ok := RunWorkspaceFromContext(WithRunWorkspace(ctx, "")); ok {
		t.Fatalf("empty workspace must not store a value, got %q", ws)
	}
	//nolint:staticcheck // SA1012: intentionally verifying nil-context tolerance
	if ws, ok := RunWorkspaceFromContext(nil); ok {
		t.Fatalf("nil ctx must report no value, got %q", ws)
	}
}

func TestRetainRunUsesCtxWorkspace(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	a.SetMemoryWorkspace("/ws/field")
	ctx := WithRunWorkspace(t.Context(), "/ws/ctx")
	a.RetainRun(ctx,
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)

	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("auto-retain must not write facts directly: %v", calls)
	}
	if wss := f.recordedEventWSs(); len(wss) != 1 || wss[0] != "/ws/ctx" {
		t.Fatalf("eventWSs = %v, want [/ws/ctx]", wss)
	}
}

func TestRetainRunCtxWorkspaceSurvivesCancellation(t *testing.T) {
	// The runner defer calls RetainRun on context.WithoutCancel(ctx); values
	// are preserved. Simulate the worst case: a fully cancelled run ctx.
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	ctx, cancel := context.WithCancel(WithRunWorkspace(t.Context(), "/ws/ctx"))
	cancel()
	a.RetainRun(context.WithoutCancel(ctx),
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil)
	if wss := f.recordedEventWSs(); len(wss) != 1 || wss[0] != "/ws/ctx" {
		t.Fatalf("eventWSs = %v, want [/ws/ctx]", wss)
	}
}

func TestRetainGoalUsesCtxWorkspace(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	a.SetMemoryWorkspace("/ws/field")
	ctx := WithRunWorkspace(t.Context(), "/ws/goal-ctx")
	a.RetainGoal(ctx, "objective", "complete", "summary", nil)
	if calls := f.recordedRememberCalls(); len(calls) != 0 {
		t.Fatalf("auto-retain must not write facts directly: %v", calls)
	}
	if wss := f.recordedEventWSs(); len(wss) != 1 || wss[0] != "/ws/goal-ctx" {
		t.Fatalf("eventWSs = %v, want [/ws/goal-ctx]", wss)
	}
}

func TestPrefetchUsesCtxWorkspace(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	a.SetMemoryWorkspace("/ws/field")
	a.beginMemoryPrefetch(WithRunWorkspace(t.Context(), "/ws/ctx"),
		[]messages.Message{messages.Human("q")})
	deadline := time.Now().Add(2 * time.Second)
	for len(f.recordedRecalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if recalls := f.recordedRecalls(); len(recalls) != 1 || recalls[0] != "/ws/ctx|q" {
		t.Fatalf("recalls = %v, want [/ws/ctx|q]", recalls)
	}
}

func TestRecordTaskEventUsesCtxWorkspace(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	a.SetMemoryWorkspace("/ws/field")
	ctx := WithRunWorkspace(t.Context(), "/ws/ctx")
	a.recordTaskEvent(ctx, "explorer", "map the auth flow", "found 3 entry points", nil)
	if wss := f.recordedEventWSs(); len(wss) != 1 || wss[0] != "/ws/ctx" {
		t.Fatalf("eventWSs = %v, want [/ws/ctx]", wss)
	}
}

func TestMemoryToolUsesCtxWorkspace(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws/workdir"}
	a.SetMemoryWorkspace("/ws/field")
	ctx := WithRunWorkspace(t.Context(), "/ws/ctx")
	tool := toolsByName(a.memoryTools())["remember_fact"]
	if _, err := tool.Invoke(ctx, map[string]any{"content": "prefers tabs"}); err != nil {
		t.Fatal(err)
	}
	if calls := f.recordedRememberCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "/ws/ctx\x00woca:tool\x00") {
		t.Fatalf("rememberCalls = %v, want workspace /ws/ctx", calls)
	}
	// recall_memory threads the same ctx workspace.
	tool2 := toolsByName(a.memoryTools())["recall_memory"]
	if _, err := tool2.Invoke(ctx, map[string]any{"query": "x"}); err != nil {
		t.Fatal(err)
	}
	if recalls := f.recordedRecalls(); len(recalls) != 1 || recalls[0] != "/ws/ctx|x" {
		t.Fatalf("recalls = %v, want [/ws/ctx|x]", recalls)
	}
}
