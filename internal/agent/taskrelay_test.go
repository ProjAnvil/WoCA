package agent

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/cpserde"
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

// --- Task 4: foreground subagent permission relay -------------------------

// transcriptModel is a transcript-driven ChatModel for the relay tests'
// subagent: runForegroundTask rebuilds the subagent (and re-binds its model)
// on every re-entry, and FakeChatModel's script cursor resets per bind copy
// — a transcript-driven fake answers from the conversation state instead,
// like a real stateless model. It returns script[len(tool results so far)],
// clamped to the last entry.
type transcriptModel struct {
	script []messages.Message
}

func (m *transcriptModel) pick(input []messages.Message) messages.Message {
	toolResults := 0
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			toolResults++
		}
	}
	idx := toolResults
	if idx >= len(m.script) {
		idx = len(m.script) - 1
	}
	return m.script[idx]
}

func (m *transcriptModel) Invoke(ctx context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *transcriptModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		resp, err := m.Invoke(ctx, in, opts...)
		if err != nil {
			return nil, err
		}
		out[i] = resp
	}
	return out, nil
}

func (m *transcriptModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *transcriptModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}

func (m *transcriptModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }

// BindTools returns the model itself: the script is transcript-driven, so a
// per-bind copy (FakeChatModel semantics) is neither needed nor wanted.
func (m *transcriptModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }

func (m *transcriptModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// relayTestAgent builds a parent agent whose model is scripted to call the
// Task tool once and then finish, paired with a transcript-driven subagent
// model (Config.SubagentModel) scripted per test. workdir roots both
// registries so the child's Write calls land in the test's temp dir.
func relayTestAgent(t *testing.T, workdir string, subScript ...messages.Message) *Agent {
	t.Helper()
	parentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Task", Args: map[string]any{
					"description": "write files",
					"prompt":      "write the files I asked for",
				}},
			}},
			messages.AI("parent done"),
		),
	)
	a, err := New(Config{
		Model:          parentModel,
		SubagentModel:  &transcriptModel{script: subScript},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeCall(id, path, content string) messages.Message {
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: id, Name: "Write", Args: map[string]any{"file_path": path, "content": content}},
	}}
}

// TestForegroundSubagentPermissionRelay: the child's Write permission pause
// must bubble out of the Task tool as a SubagentInterrupt on the PARENT
// graph; answering "y" resumes the child, its Write executes for real, and
// the child report lands in the Task tool result.
func TestForegroundSubagentPermissionRelay(t *testing.T) {
	workdir := t.TempDir()
	a := relayTestAgent(t, workdir,
		writeCall("w1", "relay.txt", "hello"),
		messages.AI("sub done"),
	)
	ctx := context.Background()
	thread := "thread-fg-relay"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	si, ok := DecodeInterruptValue(intrs[0].Value).(SubagentInterrupt)
	if !ok {
		t.Fatalf("interrupt payload = %T, want SubagentInterrupt", DecodeInterruptValue(intrs[0].Value))
	}
	if si.Subagent != "general" {
		t.Fatalf("Subagent = %q, want general", si.Subagent)
	}
	if si.Inner.ToolName != "Write" {
		t.Fatalf("Inner.ToolName = %q, want Write", si.Inner.ToolName)
	}
	if si.ToolCallID != "t1" || si.ThreadID == "" {
		t.Fatalf("ToolCallID = %q, ThreadID = %q", si.ToolCallID, si.ThreadID)
	}
	_, final := pumpStream(t, a.ResumeStream(ctx, thread, "y"))
	if !toolContentContains(final, "sub done") {
		t.Fatalf("Task result lacks the child report; final transcript: %v", final)
	}
	if _, err := os.Stat(filepath.Join(workdir, "relay.txt")); err != nil {
		t.Fatalf("child Write did not execute after approval: %v", err)
	}
}

// TestForegroundSubagentPermissionDenied: answering "n" to the bubbled pause
// aborts the subagent — the Task result carries the denial text and the
// child's Write never executes.
func TestForegroundSubagentPermissionDenied(t *testing.T) {
	workdir := t.TempDir()
	a := relayTestAgent(t, workdir,
		writeCall("w1", "denied.txt", "nope"),
		messages.AI("sub done"),
	)
	ctx := context.Background()
	thread := "thread-fg-deny"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	if _, ok := DecodeInterruptValue(intrs[0].Value).(SubagentInterrupt); !ok {
		t.Fatalf("interrupt payload = %T, want SubagentInterrupt", DecodeInterruptValue(intrs[0].Value))
	}
	_, final := pumpStream(t, a.ResumeStream(ctx, thread, "n"))
	if !toolContentContains(final, "denied") {
		t.Fatalf("Task result lacks the denial text; final transcript: %v", final)
	}
	if _, err := os.Stat(filepath.Join(workdir, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("child Write executed despite denial (stat err = %v)", err)
	}
}

// TestForegroundSubagentTwoSequentialPermissions: the child pauses on two
// Writes in a row; each pause bubbles separately and both approvals land on
// the right child interrupt.
func TestForegroundSubagentTwoSequentialPermissions(t *testing.T) {
	workdir := t.TempDir()
	a := relayTestAgent(t, workdir,
		writeCall("w1", "a.txt", "a"),
		writeCall("w2", "b.txt", "b"),
		messages.AI("sub done"),
	)
	ctx := context.Background()
	thread := "thread-fg-two"
	stream := a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread)
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
		if pauses >= 2 {
			t.Fatalf("unexpected extra pause: %v", DecodeInterruptValue(intrs[0].Value))
		}
		si, ok := DecodeInterruptValue(intrs[0].Value).(SubagentInterrupt)
		if !ok {
			t.Fatalf("pause #%d payload = %T, want SubagentInterrupt", pauses+1, DecodeInterruptValue(intrs[0].Value))
		}
		if si.Inner.ToolName != "Write" {
			t.Fatalf("pause #%d Inner.ToolName = %q, want Write", pauses+1, si.Inner.ToolName)
		}
		stream = a.ResumeStream(ctx, thread, "y")
		pauses++
	}
	if pauses != 2 {
		t.Fatalf("pauses = %d, want 2 (one per child Write)", pauses)
	}
	if !toolContentContains(final, "sub done") {
		t.Fatalf("Task result lacks the child report; final transcript: %v", final)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, name)); err != nil {
			t.Fatalf("child Write %s did not execute: %v", name, err)
		}
	}
}

// TestForegroundSubagentSequentialPermissionDenied: approving the first
// pause and denying the SECOND must abort on the second answer — the fork's
// resume queue is positional (consumed prefix + fresh answer), so a relay
// that always reads the first queued slot would replay the stale "y" and
// wrongly resume the child.
func TestForegroundSubagentSequentialPermissionDenied(t *testing.T) {
	workdir := t.TempDir()
	a := relayTestAgent(t, workdir,
		writeCall("w1", "a.txt", "a"),
		writeCall("w2", "b.txt", "b"),
		messages.AI("sub done"),
	)
	ctx := context.Background()
	thread := "thread-fg-two-deny"
	stream := a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread)
	answers := []string{"y", "n"}
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
			t.Fatalf("unexpected extra pause: %v", DecodeInterruptValue(intrs[0].Value))
		}
		if _, ok := DecodeInterruptValue(intrs[0].Value).(SubagentInterrupt); !ok {
			t.Fatalf("pause #%d payload = %T, want SubagentInterrupt", pauses+1, DecodeInterruptValue(intrs[0].Value))
		}
		stream = a.ResumeStream(ctx, thread, answers[pauses])
		pauses++
	}
	if pauses != 2 {
		t.Fatalf("pauses = %d, want 2", pauses)
	}
	if !toolContentContains(final, "denied") {
		t.Fatalf("Task result lacks the denial text; final transcript: %v", final)
	}
	if _, err := os.Stat(filepath.Join(workdir, "a.txt")); err != nil {
		t.Fatalf("approved Write a.txt did not execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied Write b.txt executed anyway (stat err = %v)", err)
	}
}

// TestResumePathsStoreLastThread: the cross-process resume entry points must
// store the resumed thread as LastThreadID — the foreground subagent relay
// derives the child thread from it, and a fresh process resuming via Resume
// (ResumeCheckpoint is an alias) or ResumeStream would otherwise derive
// "/sub-..." and respawn the child from scratch. Pauses are raised on one
// agent instance and resumed on a fresh instance over the same saver to
// mimic the process restart.
func TestResumePathsStoreLastThread(t *testing.T) {
	saver := lgcheckpoint.NewMemorySaver()
	var completions atomic.Int32
	build := func() *Agent {
		a, err := New(Config{
			Model: &transcriptModel{script: []messages.Message{
				{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "p1", Name: "pausing_tool", Args: map[string]any{}},
				}},
				messages.AI("done"),
			}},
			Registry:       tools.NewRegistry(t.TempDir()),
			ExtraTools:     []coretools.Tool{pausingTestTool(&completions)},
			SystemPrompt:   "t",
			PermissionMode: config.ModeBypass,
			Checkpointer:   saver,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ctx := context.Background()

	// Resume (also covers the ResumeCheckpoint alias).
	a1 := build()
	thread1 := "thread-resume-store"
	if intrs, _ := pumpStream(t, a1.Stream(ctx, []messages.Message{messages.Human("go")}, thread1)); len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	a2 := build()
	if got := a2.LastThreadID(); got != "" {
		t.Fatalf("fresh instance LastThreadID = %q, want empty", got)
	}
	if _, err := a2.Resume(ctx, thread1, "y"); err != nil {
		t.Fatal(err)
	}
	if got := a2.LastThreadID(); got != thread1 {
		t.Fatalf("after Resume LastThreadID = %q, want %q", got, thread1)
	}

	// ResumeStream.
	thread2 := "thread-resumestream-store"
	if intrs, _ := pumpStream(t, a1.Stream(ctx, []messages.Message{messages.Human("go")}, thread2)); len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	a3 := build()
	pumpStream(t, a3.ResumeStream(ctx, thread2, "y"))
	if got := a3.LastThreadID(); got != thread2 {
		t.Fatalf("after ResumeStream LastThreadID = %q, want %q", got, thread2)
	}
}

// TestSubagentRelayCrossProcess: the durable (sqlite) tier of the relay.
// Agent A1 pauses on the child's Write permission; the paused parent and
// child checkpoints live in a shared sqlite saver. A fresh agent A2 — new
// instance, new saver on the SAME database file, simulating a process
// restart — resumes the parent thread with "y". The relay must find the
// child's checkpoint (not respawn it), the Write executes exactly once, and
// the child report lands in the Task result.
//
// Both models are transcript-driven: a resumed run re-invokes each model on
// the rebuilt transcript, so a cursor-scripted FakeChatModel on A2 would
// replay script[0] (re-calling Task forever). The transcript models answer
// from conversation state, like a real stateless model.
func TestSubagentRelayCrossProcess(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	subScript := []messages.Message{
		writeCall("w1", "cross.txt", "hello"),
		messages.AI("sub done"),
	}
	build := func() (*Agent, *lgcpsqlite.Saver) {
		saver, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		parentModel := &transcriptModel{script: []messages.Message{
			{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Task", Args: map[string]any{
					"description": "write files",
					"prompt":      "write the files I asked for",
				}},
			}},
			messages.AI("parent done"),
		}}
		a, err := New(Config{
			Model:          parentModel,
			SubagentModel:  &transcriptModel{script: subScript},
			Registry:       tools.NewRegistry(workdir),
			SystemPrompt:   "t",
			PermissionMode: config.ModeDefault,
			WorkDir:        workdir,
			Checkpointer:   saver,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a, saver
	}
	ctx := context.Background()
	thread := "parent-thread-1"

	// Process 1: run until the child's Write permission bubbles up.
	a1, saver1 := build()
	intrs, _ := pumpStream(t, a1.Stream(ctx, []messages.Message{messages.Human("do it")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	si, ok := DecodeInterruptValue(intrs[0].Value).(SubagentInterrupt)
	if !ok {
		t.Fatalf("interrupt payload = %T, want SubagentInterrupt", DecodeInterruptValue(intrs[0].Value))
	}
	if si.Subagent != "general" || si.Inner.ToolName != "Write" || si.ToolCallID != "t1" || si.ThreadID == "" {
		t.Fatalf("envelope = %+v", si)
	}
	// The Write is gated pre-execution: no side effect may have landed yet.
	if _, err := os.Stat(filepath.Join(workdir, "cross.txt")); !os.IsNotExist(err) {
		t.Fatalf("Write executed before approval (stat err = %v)", err)
	}
	// Simulate the process exiting: the paused checkpoints are flushed and
	// the first saver connection is gone.
	if err := saver1.Close(); err != nil {
		t.Fatalf("close saver1: %v", err)
	}

	// Process 2: a fresh agent over the same database resumes the parent.
	a2, saver2 := build()
	defer func() { _ = saver2.Close() }()
	final, err := a2.Resume(ctx, thread, "y")
	if err != nil {
		// A GraphInterrupt here means the relay failed to find the child's
		// checkpoint and restarted it (the re-run Write re-paused).
		t.Fatalf("cross-process resume: %v", err)
	}
	if !toolContentContains(final, "sub done") {
		t.Fatalf("Task result lacks the child report; final transcript: %v", final)
	}
	blob, err := os.ReadFile(filepath.Join(workdir, "cross.txt"))
	if err != nil {
		t.Fatalf("child Write did not execute after cross-process approval: %v", err)
	}
	if string(blob) != "hello" {
		t.Fatalf("cross.txt = %q, want %q (Write must run exactly once)", blob, "hello")
	}
}
