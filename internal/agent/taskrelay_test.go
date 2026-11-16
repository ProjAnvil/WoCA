package agent

// Foreground-subagent test fixtures and the Task-7 deletions' survivors.
// The relay-era tests (TestForegroundSubagent*, the batch-memo suite, the
// tools-node execution-interrupt suite) were superseded by the Send fan-out
// topology: their scenarios live on as the subgraph tests in
// subgraphfanout_test.go (N1, R1, parallel same-def, cap rejection,
// background invariance) and approval_test.go (gate semantics). What remains
// here are the shared fixtures (pumpStream, transcriptModel, the probe
// executor) and the resume-entry-point pin.

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/tools"
)

func TestToolCallIDContextRoundTrip(t *testing.T) {
	ctx := withToolCallID(t.Context(), "call_42")
	if got := toolCallIDFrom(ctx); got != "call_42" {
		t.Fatalf("got %q", got)
	}
	if got := toolCallIDFrom(t.Context()); got != "" {
		t.Fatalf("empty ctx gave %q", got)
	}
	// The per-child suffix (design §3.2): a child ctx carrying the parent's
	// Task call id buckets separately from the bare run id.
	if got := specRunID(withToolCallID(t.Context(), "tA")); got != "/tA" {
		t.Fatalf("specRunID with call id = %q, want /tA", got)
	}
	if got := specRunID(t.Context()); got != "" {
		t.Fatalf("specRunID without scope = %q, want empty", got)
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

// transcriptModel is a transcript-driven ChatModel: it answers from the
// conversation state instead of a per-bind cursor, like a real stateless
// model. The fan-out runs the singleton child agent across parallel Sends
// and resume re-executions, and FakeChatModel's cursor would reset per
// context; these answer script[len(tool results so far)], clamped.
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

// fanoutTestAgent builds a parent agent whose model is scripted to call the
// Task tool once and then finish, paired with a transcript-driven subagent
// model (Config.SubagentModel) scripted per test. workdir roots both
// registries so the child's Write calls land in the test's temp dir.
func fanoutTestAgent(t *testing.T, workdir string, subScript ...messages.Message) *Agent {
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

// TestResumePathsStoreLastThread: the cross-process resume entry points must
// store the resumed thread as LastThreadID. Adapted to the subgraph shape
// (the relay rationale is gone; the pin now guards the resume pre-checks and
// any future thread-keyed bookkeeping): the pause is a subagent child's
// Write ask, raised on one agent instance and resumed on a fresh instance
// over the same saver to mimic the process restart.
func TestResumePathsStoreLastThread(t *testing.T) {
	saver := lgcheckpoint.NewMemorySaver()
	build := func() *Agent {
		a, err := New(Config{
			Model: &transcriptModel{script: []messages.Message{
				{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "t1", Name: "Task", Args: map[string]any{
						"description": "write files",
						"prompt":      "write the files I asked for",
					}},
				}},
				messages.AI("parent done"),
			}},
			SubagentModel: &transcriptModel{script: []messages.Message{
				writeCall("w1", "thread.txt", "x"),
				messages.AI("sub done"),
			}},
			Registry:       tools.NewRegistry(t.TempDir()),
			SystemPrompt:   "t",
			PermissionMode: config.ModeDefault,
			Checkpointer:   saver,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ctx := t.Context()

	// Resume (also covers the ResumeCheckpoint alias).
	a1 := build()
	thread1 := "thread-resume-store"
	intrs, _ := pumpStream(t, a1.Stream(ctx, []messages.Message{messages.Human("do it")}, thread1))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1 (the child's Write ask)", len(intrs))
	}
	if _, ok := DecodeInterruptValue(intrs[0].Value).(ApprovalRequest); !ok {
		t.Fatalf("pause payload = %T, want the child's ApprovalRequest", DecodeInterruptValue(intrs[0].Value))
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

// relaySiblingMarker is the output marker of the probe command the R1
// revival's speculated sibling runs.
const relaySiblingMarker = "RELAY-SIBLING-RESULT"

// relayProbeExec wraps the local executor for the R1 revival: executions of
// the sibling probe command are observed (completion vs cancellation) so the
// test can count how many times the child's speculated sibling actually ran.
// The subagent inherits the parent's executor (newSubagent), so the child's
// Bash calls are observable here without custom child tools.
type relayProbeExec struct {
	executor.Executor
	mu     sync.Mutex
	done   int // probe executions that ran to completion
	killed int // probe executions whose ctx was cancelled first
}

func (e *relayProbeExec) Exec(ctx context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	if req.Name != "bash" || len(req.Args) != 2 || !strings.Contains(req.Args[1], relaySiblingMarker) {
		return e.Executor.Exec(ctx, req)
	}
	res, err := e.Executor.Exec(ctx, req)
	e.mu.Lock()
	if err != nil && ctx.Err() != nil {
		e.killed++
	} else {
		e.done++
	}
	e.mu.Unlock()
	return res, err
}

func (e *relayProbeExec) stats() (done, killed int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done, e.killed
}

// waitProbeDone polls until n probe executions completed (bounded).
func waitProbeDone(t *testing.T, e *relayProbeExec, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if done, _ := e.stats(); done >= n {
			return
		}
		if time.Now().After(deadline) {
			done, killed := e.stats()
			t.Fatalf("probe completions = %d (killed %d), want >= %d", done, killed, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// subReportModel is the R1 child model: the first round returns the scripted
// tool-call batch; every later round reports every tool result the child's
// tools node produced, so the test can assert exactly what the child
// consumed for its speculated sibling.
type subReportModel struct {
	batch messages.Message
}

func (m *subReportModel) pick(input []messages.Message) messages.Message {
	toolResults := 0
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			toolResults++
		}
	}
	if toolResults == 0 {
		return m.batch
	}
	var b strings.Builder
	b.WriteString("sub report")
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			b.WriteString("\n[" + msg.ToolCallID + "] " + msg.Content)
		}
	}
	return messages.AI(b.String())
}

func (m *subReportModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *subReportModel) Batch(_ context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		resp, _ := m.Invoke(context.Background(), in, opts...)
		out[i] = resp
	}
	return out, nil
}

func (m *subReportModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *subReportModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *subReportModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }

// BindTools returns the model itself (transcript-driven; see transcriptModel).
func (m *subReportModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }

func (m *subReportModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}
