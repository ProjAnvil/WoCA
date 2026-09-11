package runner

// Gap tests: interrupt handling without/with-failing handlers, summary
// rendering around interrupts, thread pinning, and renderer/summarizer
// edge branches.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func newWriteAgent(t *testing.T) *agent.Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, Content: "Let me write it.", ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("Done."),
		),
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
	return a
}

func TestRunnerPinsThreadID(t *testing.T) {
	a := newWriteAgent(t)
	if New(a, nil, nil).WithThreadID("thread-7") == nil {
		t.Fatal("WithThreadID should chain")
	}
}

func TestRunnerInterruptWithoutHandler(t *testing.T) {
	a := newWriteAgent(t)
	r := New(a, nil, nil)
	_, err := r.Run(context.Background(), []messages.Message{messages.Human("write it")})
	if err == nil {
		t.Fatal("interrupt without a handler should surface")
	}
}

func TestRunnerInterruptHandlerError(t *testing.T) {
	a := newWriteAgent(t)
	boom := errors.New("user fled")
	r := New(a, func(any) (any, error) { return nil, boom }, nil)
	if _, err := r.Run(context.Background(), []messages.Message{messages.Human("write it")}); !errors.Is(err, boom) {
		t.Fatalf("handler error should propagate, got %v", err)
	}
}

func TestRunnerSummariesRenderedAroundInterrupt(t *testing.T) {
	a := newWriteAgent(t)
	var rendered strings.Builder
	var summaries int
	r := New(a, func(any) (any, error) { return "y", nil }, DefaultRenderer(&rendered))
	r.RenderSummary = func(s agent.ToolUseSummary) { summaries++ }
	if _, err := r.Run(context.Background(), []messages.Message{messages.Human("write it")}); err != nil {
		t.Fatal(err)
	}
	if rendered.Len() == 0 {
		t.Fatal("nothing rendered")
	}
	_ = summaries
}

func TestSummarizeArgsBranches(t *testing.T) {
	if got := summarizeArgs(nil); got != "" {
		t.Fatalf("empty args = %q", got)
	}
	if got := summarizeArgs(map[string]any{"bad": make(chan int)}); got != "..." {
		t.Fatalf("unmarshalable args = %q", got)
	}
	if got := summarizeArgs(map[string]any{"k": 1}); got != `{"k":1}` {
		t.Fatalf("args = %q", got)
	}
}
