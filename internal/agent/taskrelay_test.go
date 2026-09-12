package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func TestDeriveSubagentThreadDeterministic(t *testing.T) {
	a := deriveSubagentThread("run-1", "call_9", "do the thing")
	b := deriveSubagentThread("run-1", "call_9", "do the thing")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "run-1/sub-call_9-") {
		t.Fatalf("shape = %q", a)
	}
	// 同 prompt 不同 call ID 不碰撞;无 call ID 时退化为 prompt 摘要。
	if c := deriveSubagentThread("run-1", "call_10", "do the thing"); c == a {
		t.Fatal("different call IDs must not collide")
	}
	if d := deriveSubagentThread("run-1", "", "do the thing"); !strings.HasPrefix(d, "run-1/sub-") {
		t.Fatalf("no-callID fallback shape = %q", d)
	}
}

func TestToolCallIDContextRoundTrip(t *testing.T) {
	ctx := withToolCallID(context.Background(), "call_42")
	if got := toolCallIDFrom(ctx); got != "call_42" {
		t.Fatalf("got %q", got)
	}
	if got := toolCallIDFrom(context.Background()); got != "" {
		t.Fatalf("empty ctx gave %q", got)
	}
}

// chunkInterrupts extracts the pause interrupts from an updates- or
// values-chunk payload, or nil when the chunk is not a pause (mirrors the
// runner's chunkInterrupts).
func chunkInterrupts(chunk lggraph.StreamChunk) []lgtypes.Interrupt {
	var v any
	switch chunk.Mode {
	case lggraph.StreamUpdates, lggraph.StreamValues:
		v = chunk.Payload
	default:
		return nil
	}
	m, _ := v.(map[string]any)
	intrs, _ := m[lgcheckpoint.ReservedInterrupt].([]lgtypes.Interrupt)
	return intrs
}

// TestToolsNodeInterruptSkipsCompletedCalls: a batch [countingTool,
// pausingTool] pauses mid-execution (the pausing tool's GraphInterrupt
// panic must bubble out of the tools node, not be swallowed into an error
// message); on re-execution the counting tool must not re-run — its
// completed result is memoized and its permission gate not re-asked.
func TestToolsNodeInterruptSkipsCompletedCalls(t *testing.T) {
	var runs atomic.Int32
	counting, err := coretools.FromFunc("counting_tool", "test counting tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			runs.Add(1)
			return coretools.Result{Content: "ok"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	pausing, err := coretools.FromFunc("pausing_tool", "test pausing tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			// Interrupt with the tool's own ctx (it carries the graph's
			// interrupt state); on resume this returns the resume value.
			v, _ := lggraph.Interrupt(ctx, "pause-here").(string)
			return coretools.Result{Content: "resumed:" + v}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "counting_tool", Args: map[string]any{}},
				{ID: "c2", Name: "pausing_tool", Args: map[string]any{}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		ExtraTools:     []coretools.Tool{counting, pausing},
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	thread := "thread-toolsnode-interrupt"
	sawInterrupt := false
	for chunk, err := range a.Stream(ctx, []messages.Message{messages.Human("run the tools")}, thread) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, in := range chunkInterrupts(chunk) {
			if got := decodeInterruptPayload(in.Value); got != "pause-here" {
				t.Fatalf("interrupt value = %v, want pause-here", got)
			}
			sawInterrupt = true
		}
	}
	if !sawInterrupt {
		t.Fatal("stream did not pause on the pausing tool's interrupt")
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("counting tool ran %d times before the pause, want 1", got)
	}
	for _, err := range a.ResumeStream(ctx, thread, "y") {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("counting tool ran %d times after resume, want exactly 1 (memoized)", got)
	}
}
