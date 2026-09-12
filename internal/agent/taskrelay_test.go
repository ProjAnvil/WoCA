package agent

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/permissions"
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

// pumpStream drains a chunk stream, collecting pause interrupts and the
// latest values payload's messages, failing the test on a stream error. A
// pause is reported under both the updates and values modes; only the first
// report is kept.
func pumpStream(t *testing.T, seq iter.Seq2[lggraph.StreamChunk, error]) (intrs []lgtypes.Interrupt, vals []messages.Message) {
	t.Helper()
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if len(intrs) == 0 {
			intrs = chunkInterrupts(chunk)
		}
		if chunk.Mode == lggraph.StreamValues {
			if m, ok := chunk.Payload.(map[string]any); ok {
				if msgs := MessagesFrom(m); len(msgs) > 0 {
					vals = msgs
				}
			}
		}
	}
	return intrs, vals
}

// toolContentContains reports whether any tool message carries the substring.
func toolContentContains(msgs []messages.Message, want string) bool {
	for _, m := range msgs {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// pausingTestTool interrupts mid-execution with "pause-here" and, once
// resumed, embeds the received resume value in its result so tests can
// assert which queue slot it consumed.
func pausingTestTool(completions *atomic.Int32) coretools.Tool {
	t, err := coretools.FromFunc("pausing_tool", "test pausing tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			// Interrupt with the tool's own ctx (it carries the graph's
			// interrupt state); on resume this returns the resume value.
			v, _ := lggraph.Interrupt(ctx, "pause-here").(string)
			completions.Add(1)
			return coretools.Result{Content: "resumed:" + v}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// TestToolsNodeInterruptSkipsCompletedCalls: a batch [countingTool,
// pausingTool] pauses mid-execution (the pausing tool's GraphInterrupt
// panic must bubble out of the tools node, not be swallowed into an error
// message); on re-execution the counting tool must not re-run — its
// completed result is memoized and its permission gate not re-asked — and
// the pausing tool must receive the resume value.
func TestToolsNodeInterruptSkipsCompletedCalls(t *testing.T) {
	var runs, completions atomic.Int32
	counting, err := coretools.FromFunc("counting_tool", "test counting tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			runs.Add(1)
			return coretools.Result{Content: "ok"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	pausing := pausingTestTool(&completions)
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
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("run the tools")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	if got := decodeInterruptPayload(intrs[0].Value); got != "pause-here" {
		t.Fatalf("interrupt value = %v, want pause-here", got)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("counting tool ran %d times before the pause, want 1", got)
	}
	_, final := pumpStream(t, a.ResumeStream(ctx, thread, "y"))
	if got := runs.Load(); got != 1 {
		t.Fatalf("counting tool ran %d times after resume, want exactly 1 (memoized)", got)
	}
	if got := completions.Load(); got != 1 {
		t.Fatalf("pausing tool completed %d times, want 1", got)
	}
	// The resume value must reach the pausing tool (queue slot aligned).
	if !toolContentContains(final, "resumed:y") {
		t.Fatalf("final transcript lacks the pausing tool's resumed result: %v", final)
	}
}

// TestToolsNodeMemoReplayKeepsResumeAlignment: a batch [gatedTool,
// pausingTool] in default mode pauses twice — the gate's permission ask for
// the gated tool, then the pausing tool's execution-phase interrupt. On the
// second re-execution the gated tool is memoized and skips its gate; its
// earlier Interrupt consumption must be replayed so the pausing tool's
// Interrupt consumes the second resume value, not the permission answer.
func TestToolsNodeMemoReplayKeepsResumeAlignment(t *testing.T) {
	var gatedRuns, completions atomic.Int32
	gated, err := coretools.FromFunc("counting_tool", "test counting tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			gatedRuns.Add(1)
			return coretools.Result{Content: "gated-ok"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	pausing := pausingTestTool(&completions)
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "g1", Name: "counting_tool", Args: map[string]any{}},
				{ID: "p1", Name: "pausing_tool", Args: map[string]any{}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		ExtraTools:     []coretools.Tool{gated, pausing},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		// counting_tool is unknown (CatShell) → the default mode asks;
		// pausing_tool is allow-ruled → its gate never interrupts, so the
		// only gate interrupt is the counting tool's ask.
		Permissions: &permissions.Rules{Allow: []string{"pausing_tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	thread := "thread-toolsnode-memo-replay"
	stream := a.Stream(ctx, []messages.Message{messages.Human("go")}, thread)
	pauses := 0
	var final []messages.Message
	for {
		intrs, vals := pumpStream(t, stream)
		if len(vals) > 0 {
			final = vals
		}
		if len(intrs) == 0 {
			break
		}
		pauses++
		answer := "r2"
		if req, ok := decodeInterruptPayload(intrs[0].Value).(PermissionRequest); ok {
			if req.ToolName != "counting_tool" {
				t.Fatalf("permission ask for %q, want counting_tool", req.ToolName)
			}
			answer = "y"
		}
		stream = a.ResumeStream(ctx, thread, answer)
	}
	if pauses != 2 {
		t.Fatalf("pauses = %d, want 2 (one permission ask, one execution interrupt)", pauses)
	}
	if got := gatedRuns.Load(); got != 1 {
		t.Fatalf("gated tool ran %d times, want exactly 1 (memoized)", got)
	}
	if got := completions.Load(); got != 1 {
		t.Fatalf("pausing tool completed %d times, want 1", got)
	}
	// Queue: ["y" (gate ask), "r2" (execution interrupt)]. Misalignment would
	// hand the pausing tool "y"; the memo replay must keep it on "r2".
	if !toolContentContains(final, "resumed:r2") {
		t.Fatalf("pausing tool consumed the wrong resume value; final transcript: %v", final)
	}
	if toolContentContains(final, "resumed:y") {
		t.Fatal("pausing tool consumed the permission answer — resume queue misaligned")
	}
}

// TestToolsNodeMemoReplayCoversExecutionInterrupts: a batch [pauseX, pauseY]
// (bypass mode, no gate interrupts) pauses twice — X mid-execution, then Y
// mid-execution after X resumed and completed. X's memo entry must carry its
// execution-phase Interrupt consumption, so on the final re-execution X's
// memo hit replays it and Y's Interrupt consumes its own resume value ("vy")
// instead of the already-spent "vx".
//
// The execution goroutines run concurrently, so the test must order the two
// Interrupt calls deterministically: Y waits on xConsumed (closed when X's
// Interrupt returns, i.e. after X consumed its resume value) before calling
// its own. In the first execution X panics before closing the channel, so Y
// falls through a timeout and interrupts too — its panic is stashed and only
// X's (index 0) is re-panicked, yielding pause #1 = X.
func TestToolsNodeMemoReplayCoversExecutionInterrupts(t *testing.T) {
	var xDone, yDone atomic.Int32
	xConsumed := make(chan struct{})
	pauseX, err := coretools.FromFunc("pause_x", "test pausing tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			v, _ := lggraph.Interrupt(ctx, "pause-x").(string)
			close(xConsumed)
			xDone.Add(1)
			return coretools.Result{Content: "pause_x:" + v}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	pauseY, err := coretools.FromFunc("pause_y", "test pausing tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			select {
			case <-xConsumed:
			case <-time.After(500 * time.Millisecond):
			}
			v, _ := lggraph.Interrupt(ctx, "pause-y").(string)
			yDone.Add(1)
			return coretools.Result{Content: "pause_y:" + v}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "x1", Name: "pause_x", Args: map[string]any{}},
				{ID: "y1", Name: "pause_y", Args: map[string]any{}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		ExtraTools:     []coretools.Tool{pauseX, pauseY},
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	thread := "thread-toolsnode-exec-replay"
	wantPause := []string{"pause-x", "pause-y"}
	answers := []string{"vx", "vy"}
	stream := a.Stream(ctx, []messages.Message{messages.Human("go")}, thread)
	pauses := 0
	var final []messages.Message
	for {
		intrs, vals := pumpStream(t, stream)
		if len(vals) > 0 {
			final = vals
		}
		if len(intrs) == 0 {
			break
		}
		if pauses >= len(answers) {
			t.Fatalf("unexpected extra pause: %v", decodeInterruptPayload(intrs[0].Value))
		}
		if got := decodeInterruptPayload(intrs[0].Value); got != wantPause[pauses] {
			t.Fatalf("pause #%d value = %v, want %s", pauses+1, got, wantPause[pauses])
		}
		stream = a.ResumeStream(ctx, thread, answers[pauses])
		pauses++
	}
	if pauses != 2 {
		t.Fatalf("pauses = %d, want 2 (X's then Y's execution interrupt)", pauses)
	}
	if got := xDone.Load(); got != 1 {
		t.Fatalf("pause_x completed %d times, want 1", got)
	}
	if got := yDone.Load(); got != 1 {
		t.Fatalf("pause_y completed %d times, want 1", got)
	}
	// Queue: ["vx" (X), "vy" (Y)]. Without replaying X's execution-phase
	// consumption, Y's memo-aligned Interrupt would consume "vx".
	if !toolContentContains(final, "pause_x:vx") {
		t.Fatalf("X's result missing or wrong; final transcript: %v", final)
	}
	if !toolContentContains(final, "pause_y:vy") {
		t.Fatalf("Y consumed the wrong resume value; final transcript: %v", final)
	}
}
