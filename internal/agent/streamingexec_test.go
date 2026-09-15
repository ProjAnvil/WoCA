package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// timedChunk is one streamed chunk preceded by a delay, letting tests model
// a model that keeps streaming after emitting a tool call.
type timedChunk struct {
	before time.Duration
	chunk  messages.Message
}

// scriptedModel is a ChatModel whose Stream plays back per-turn chunk
// scripts with delays. BindTools preserves the script so the agent's tool
// binding keeps the streaming behavior.
type scriptedModel struct {
	caps  language.ChatModelCapabilities
	state *scriptState
}

type scriptState struct {
	mu    sync.Mutex
	turns [][]timedChunk
	next  int
	ended []time.Time // per-turn stream end timestamps
}

func (m *scriptedModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.AI("scripted"), nil
}

func (m *scriptedModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i := range inputs {
		out[i] = messages.AI("scripted")
	}
	return out, nil
}

func (m *scriptedModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &scriptedStream{state: m.state}, nil
}

func (m *scriptedModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *scriptedModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}

func (m *scriptedModel) BindTools(boundTools []coretools.Tool) (language.ChatModel, error) {
	if len(boundTools) > 0 && !m.caps.ToolCalling {
		return nil, fmt.Errorf("tool calling is not supported")
	}
	return &scriptedModel{caps: m.caps, state: m.state}, nil
}

func (m *scriptedModel) Capabilities() language.ChatModelCapabilities { return m.caps }

// scriptedStream plays back one turn's script, recording when the stream
// ended (first exhausted Next call).
type scriptedStream struct {
	state   *scriptState
	chunks  []timedChunk
	i       int
	claimed bool
	endIdx  int
}

func (s *scriptedStream) claim() {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.next < len(s.state.turns) {
		s.chunks = s.state.turns[s.state.next]
		s.endIdx = s.state.next
		s.state.next++
	}
}

func (s *scriptedStream) Next(ctx context.Context) (messages.Message, bool, error) {
	if !s.claimed {
		s.claimed = true
		s.claim()
	}
	if s.i >= len(s.chunks) {
		s.state.mu.Lock()
		if s.endIdx < len(s.state.ended) && s.state.ended[s.endIdx].IsZero() {
			s.state.ended[s.endIdx] = time.Now()
		}
		s.state.mu.Unlock()
		return messages.Message{}, false, nil
	}
	tc := s.chunks[s.i]
	s.i++
	if tc.before > 0 {
		select {
		case <-time.After(tc.before):
		case <-ctx.Done():
			return messages.Message{}, false, ctx.Err()
		}
	}
	return tc.chunk, true, nil
}

func (s *scriptedStream) Close() error { return nil }

// recorder tracks tool executions across the speculative path.
type recorder struct {
	mu     sync.Mutex
	starts []time.Time
	ends   []time.Time
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *recorder) firstStart() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.starts) == 0 {
		return time.Time{}
	}
	return r.starts[0]
}

// sleepTool registers a "Sleep" tool that sleeps then reports, recording
// start/end times.
func sleepTool(rec *recorder) coretools.Tool { return sleepToolNamed("Sleep", rec) }

// sleepToolNamed is sleepTool under a chosen name, so tests can reuse a
// known category (e.g. "Grep" → CatRead → auto-allowed in default mode).
func sleepToolNamed(name string, rec *recorder) coretools.Tool {
	t, _ := coretools.FromFunc(name, "Sleep for ms milliseconds (test tool)",
		func(ctx context.Context, args struct {
			MS int `json:"ms"`
		}) (coretools.Result, error) {
			rec.mu.Lock()
			rec.starts = append(rec.starts, time.Now())
			rec.mu.Unlock()
			select {
			case <-time.After(time.Duration(args.MS) * time.Millisecond):
			case <-ctx.Done():
				rec.mu.Lock()
				rec.ends = append(rec.ends, time.Now())
				rec.mu.Unlock()
				return coretools.Result{Content: "cancelled"}, nil
			}
			rec.mu.Lock()
			rec.ends = append(rec.ends, time.Now())
			rec.mu.Unlock()
			return coretools.Result{Content: "slept"}, nil
		})
	return t
}

// TestStreamingToolExecutionOverlaps verifies the core streaming-tool-execution
// property: a tool call that arrives mid-stream starts executing before the
// model's stream ends, and the transcript stays correct with no duplicate
// executions.
func TestStreamingToolExecutionOverlaps(t *testing.T) {
	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Grep", rec))

	state := &scriptState{
		turns: [][]timedChunk{
			{
				// First chunk seeds the message.
				{0, func() messages.Message {
					m := messages.AI("")
					m.Content = "checking "
					return m
				}()},
				// The complete tool call arrives here; execution should start
				// immediately while the next chunk is still 400ms away.
				{0, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "t1", Name: "Grep", Args: map[string]any{"ms": 250}},
				}}},
				{400 * time.Millisecond, func() messages.Message {
					m := messages.AI("")
					m.Content = "still streaming"
					return m
				}()},
			},
			{
				{0, messages.AI("done")},
			},
		},
		ended: make([]time.Time, 2),
	}
	model := &scriptedModel{
		caps:  language.ChatModelCapabilities{ToolCalling: true, Streaming: true},
		state: state,
	}

	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("sleep please")}, "t")
	if err != nil {
		t.Fatal(err)
	}

	if got := rec.count(); got != 1 {
		t.Fatalf("Sleep executed %d times, want exactly 1", got)
	}

	// The overlap assertion: the tool must have started strictly before the
	// model's first turn finished streaming (the trailing chunk arrives
	// 400ms after the tool call chunk, and the tool sleeps 250ms).
	state.mu.Lock()
	streamEnd := state.ended[0]
	state.mu.Unlock()
	toolStart := rec.firstStart()
	if streamEnd.IsZero() {
		t.Fatal("stream end not recorded")
	}
	if !toolStart.Before(streamEnd) {
		t.Fatalf("tool started at %v, after stream end %v — no overlap", toolStart, streamEnd)
	}
	if d := streamEnd.Sub(toolStart); d < 100*time.Millisecond {
		t.Fatalf("overlap only %v; want the tool well underway before stream end", d)
	}

	// Transcript shape: system, human, AI with the tool call + merged text,
	// tool result, final AI.
	last := out[len(out)-1]
	if last.Role != messages.RoleAI || last.Content != "done" {
		t.Fatalf("final message = %+v", last)
	}
	var toolMsg *messages.Message
	for i := range out {
		if out[i].Role == messages.RoleTool && out[i].ToolCallID == "t1" {
			toolMsg = &out[i]
		}
	}
	if toolMsg == nil || toolMsg.Content != "slept" {
		t.Fatalf("tool result missing or wrong: %+v", toolMsg)
	}
}

// TestStreamingExecutionSkipsGatedCalls verifies that calls needing the
// permission ask are NOT pre-started: the graph still interrupts with a
// PermissionRequest and the tool has not executed.
func TestStreamingExecutionSkipsGatedCalls(t *testing.T) {
	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepTool(rec))

	state := &scriptState{
		turns: [][]timedChunk{
			{
				{0, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
				}}},
				{150 * time.Millisecond, func() messages.Message {
					m := messages.AI("")
					m.Content = "trailing text"
					return m
				}()},
			},
		},
		ended: make([]time.Time, 1),
	}
	model := &scriptedModel{
		caps:  language.ChatModelCapabilities{ToolCalling: true, Streaming: true},
		state: state,
	}

	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("write a file")}, "t")
	ie := interruptFromError(t, err)
	if req, ok := ie.Value.(PermissionRequest); !ok || req.ToolName != "Write" {
		t.Fatalf("interrupt value = %+v", ie.Value)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("Sleep ran %d times; speculative path must not run anything here", got)
	}

	// Approve: the run completes and the write happened exactly once.
	if _, err := a.Resume(t.Context(), "t", "y"); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("Sleep unexpectedly ran %d times", got)
	}
}

// TestStreamingExecutionReadOnlyDefaultMode verifies a read-only tool call
// is pre-started even in the default permission mode: a recording "Grep"
// (CatRead, auto-allowed) must begin before the model's stream ends.
func TestSpeculativeAllowedReadOnlyGate(t *testing.T) {
	for _, mode := range []config.PermissionMode{config.ModeDefault, config.ModeAcceptEdits, config.ModeBypass, config.ModePlan, config.ModeReadOnly} {
		a := &Agent{mode: mode, perms: nil}
		write := messages.ToolCall{ID: "1", Name: "Write", Args: map[string]any{"path": "x.go", "content": "package x"}}
		if a.speculativeAllowed(write) {
			t.Fatalf("mode %v: write tool speculatively dispatched, want blocked", mode)
		}
		read := messages.ToolCall{ID: "2", Name: "Read", Args: map[string]any{"path": "x.go"}}
		if !a.speculativeAllowed(read) {
			t.Fatalf("mode %v: read tool blocked, want allowed", mode)
		}
		// CatMeta tools with irreversible side effects are excluded from
		// speculative dispatch: Task spawns a subagent and ScheduleCron
		// registers a cron — a restarted model stream would run them twice.
		for _, name := range []string{"Task", "ScheduleCron"} {
			side := messages.ToolCall{ID: "3", Name: name, Args: map[string]any{}}
			if a.speculativeAllowed(side) {
				t.Fatalf("mode %v: %s speculatively dispatched, want blocked", mode, name)
			}
		}
		// The remaining CatMeta tools keep the auto-allow behavior.
		meta := messages.ToolCall{ID: "4", Name: "TaskList", Args: map[string]any{}}
		if !a.speculativeAllowed(meta) {
			t.Fatalf("mode %v: TaskList blocked, want allowed", mode)
		}
	}
}

func TestStreamingExecutionReadOnlyDefaultMode(t *testing.T) {
	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Grep", rec))

	state := &scriptState{
		turns: [][]timedChunk{
			{
				{0, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "r1", Name: "Grep", Args: map[string]any{"ms": 250}},
				}}},
				{400 * time.Millisecond, func() messages.Message {
					m := messages.AI("")
					m.Content = "done thinking"
					return m
				}()},
			},
			{
				{0, messages.AI("finished")},
			},
		},
		ended: make([]time.Time, 2),
	}
	model := &scriptedModel{
		caps:  language.ChatModelCapabilities{ToolCalling: true, Streaming: true},
		state: state,
	}

	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("search it")}, "t")
	if err != nil {
		t.Fatal(err)
	}

	if got := rec.count(); got != 1 {
		t.Fatalf("Grep executed %d times, want exactly 1", got)
	}
	// The speculative call must start before the turn-1 stream ends (the
	// trailing text chunk delays the end by 400ms; the tool sleeps 250ms).
	state.mu.Lock()
	streamEnd := state.ended[0]
	state.mu.Unlock()
	if streamEnd.IsZero() {
		t.Fatal("stream end not recorded")
	}
	if toolStart := rec.firstStart(); !toolStart.Before(streamEnd) {
		t.Fatalf("tool started at %v, after stream end %v — no overlap", toolStart, streamEnd)
	}
	var toolMsg *messages.Message
	for i := range out {
		if out[i].Role == messages.RoleTool && out[i].ToolCallID == "r1" {
			toolMsg = &out[i]
		}
	}
	if toolMsg == nil || toolMsg.Content != "slept" {
		t.Fatalf("tool result missing or wrong: %+v", toolMsg)
	}
	if last := out[len(out)-1]; last.Content != "finished" {
		t.Fatalf("final message = %+v", last)
	}
}
