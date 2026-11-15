package agent

// Regression pin for consent claim N5 of the relay idempotency analysis
// (2026-11-08): a guard-escalated permission ask must be final. Revived from
// the analysis-round archive; the shared fixtures (memoProbeTool, pumpStream)
// live in taskrelay_test.go, logToolMsgs in relayidem_helpers_test.go.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

// askGuard escalates one tool to the permission prompt (ToolAsk).
type askGuard struct{ name string }

func (g askGuard) BeforeTool(_ context.Context, call ToolCallInfo) ToolDecision {
	if call.Name == g.name {
		return AskTool()
	}
	return AllowTool()
}

func (askGuard) AfterTool(context.Context, ToolCallInfo, ToolResultInfo) ToolPostDecision {
	return ToolPostDecision{}
}

// N5 — a guard escalates memo_probe to ToolAsk; the user answers "n"; the gate
// loop lacks a `continue`, so the allow-ruled gate re-sets run[i]=true.
// Want: memo_probe must not execute.
func TestReproN5GuardAskDenialOverridden(t *testing.T) {
	var n atomic.Int32
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "memo_probe", Args: map[string]any{}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		ExtraTools:     []coretools.Tool{memoProbeTool(&n)},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"memo_probe"}},
		ToolGuards:     []ToolGuard{askGuard{name: "memo_probe"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-repro-n5"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("go")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1 (the guard's ask)", len(intrs))
	}
	_, final := pumpStream(t, a.ResumeStream(ctx, thread, "n"))
	logToolMsgs(t, "final", final)
	t.Logf("memo_probe executions after the user's \"n\": %d", n.Load())
	if got := n.Load(); got != 0 {
		t.Errorf("CONSENT OVERRIDE: guard-escalated call executed %d time(s) after the user denied it", got)
	}
}
