package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/model"
	"github.com/projanvil/woca/internal/tools"
)

// agentStreamScript is one Stream() call: chunks in order, then (when
// failAfter >= 0) a failure after that many chunks.
type agentStreamScript struct {
	chunks    []messages.Message
	failAfter int
	err       error
}

// scheduledModel plays scripts in order, one per Stream() call. Invoke
// returns invokeText so the same type can also serve as a summarizer.
type scheduledModel struct {
	mu         sync.Mutex
	scripts    []agentStreamScript
	calls      int
	invokeText string
}

func (m *scheduledModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scheduledModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	text := m.invokeText
	if text == "" {
		text = "summary of the conversation"
	}
	return messages.AI(text), nil
}

func (m *scheduledModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, errors.New("batch not used")
}

func (m *scheduledModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	i := m.calls
	m.calls++
	m.mu.Unlock()
	script := agentStreamScript{failAfter: -1}
	if i < len(m.scripts) {
		script = m.scripts[i]
	}
	return &scheduledStream{script: script}, nil
}

func (m *scheduledModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *scheduledModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *scheduledModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *scheduledModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

type scheduledStream struct {
	script agentStreamScript
	i      int
}

func (s *scheduledStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.script.failAfter >= 0 && s.i >= s.script.failAfter {
		return messages.Message{}, false, s.script.err
	}
	if s.i >= len(s.script.chunks) {
		return messages.Message{}, false, nil
	}
	chunk := s.script.chunks[s.i]
	s.i++
	return chunk, true, nil
}

func (s *scheduledStream) Close() error { return nil }

// TestStreamFallbackCleanMessage verifies the agent-level semantics of a
// mid-stream model fallback (Claude Code's FallbackTriggeredError handling):
// the failed attempt's partial text is discarded from the final message,
// speculative tool calls from the failed attempt are cancelled, and the
// fallback attempt's tool call runs exactly once.
func TestStreamFallbackCleanMessage(t *testing.T) {
	errOverload := errors.New("529 overloaded_error")

	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Sleep", rec))

	failChunk := func(id string) messages.Message {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: id, Name: "Sleep", Args: map[string]any{"ms": 900}},
		}}
	}
	text := func(s string) messages.Message {
		m := messages.AI("")
		m.Content = s
		return m
	}

	// Primary turn 1 fails twice mid-stream (after text + a speculative tool
	// call); turn 2 succeeds on the primary after the router's per-Stream
	// reset. The old Sleep (900ms) never completes before its cancellation.
	primary := &scheduledModel{scripts: []agentStreamScript{
		{failAfter: 2, chunks: []messages.Message{text("checking "), failChunk("old1")}, err: errOverload},
		{failAfter: 2, chunks: []messages.Message{text("checking "), failChunk("old2")}, err: errOverload},
		{failAfter: -1, chunks: []messages.Message{text("done")}},
	}}
	// Fallback turn 1 recovers with its own tool call and text.
	fallback := &scheduledModel{scripts: []agentStreamScript{
		{failAfter: -1, chunks: []messages.Message{
			failChunk2("new1", 50),
			text("recovered"),
		}},
	}}

	var fallbackNotified int
	router, err := model.New(model.Config{
		Primary:    primary,
		Fallback:   fallback,
		MaxRetries: 1,
		OnFallback: func(error) { fallbackNotified++ },
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := New(Config{
		Model:          router,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}

	// The failed attempts' partial text never reaches the final transcript,
	// and the fallback's turn-1 message carries only its own tool call.
	var aiWithTool *messages.Message
	var toolMsg *messages.Message
	for i := range out {
		m := out[i]
		if m.Role == messages.RoleAI && len(m.ToolCalls) > 0 && aiWithTool == nil {
			aiWithTool = &out[i]
		}
		if m.Role == messages.RoleTool && m.ToolCallID == "new1" {
			toolMsg = &out[i]
		}
	}
	if aiWithTool == nil {
		t.Fatal("no tool-carrying AI message in transcript")
	}
	if aiWithTool.Content != "recovered" || len(aiWithTool.ToolCalls) != 1 || aiWithTool.ToolCalls[0].ID != "new1" {
		t.Fatalf("fallback AI message = %+v", *aiWithTool)
	}
	if toolMsg == nil || toolMsg.Content != "slept" {
		t.Fatalf("tool result = %+v, want new1 -> slept", toolMsg)
	}
	if last := out[len(out)-1]; last.Role != messages.RoleAI || last.Content != "done" {
		t.Fatalf("final message = %+v", last)
	}

	// Both failed attempts started a speculative Sleep that was cancelled;
	// the fallback attempt's call completed. (The 900ms sleeps outlive the
	// ~1s of retries only until their cancellation, never completing.)
	if got := rec.count(); got != 3 {
		t.Fatalf("Sleep started %d times, want 3 (2 cancelled + 1 completed)", got)
	}
	if fallbackNotified != 1 {
		t.Fatalf("onFallback called %d times, want 1", fallbackNotified)
	}
	// The registry is fully drained after the turn.
	if f := a.spec.get("new1"); f != nil {
		t.Fatal("speculative registry still holds the consumed call")
	}
}

// failChunk2 builds a Sleep tool-call chunk with an explicit duration.
func failChunk2(id string, ms int) messages.Message {
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: id, Name: "Sleep", Args: map[string]any{"ms": ms}},
	}}
}
