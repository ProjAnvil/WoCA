package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// spyModel records every model input and delegates to an inner model — used
// to assert what a forked subagent actually received. BindTools returns a
// bound spy sharing the same recording.
type spyModel struct {
	inner language.ChatModel
	rec   *spyRecorder
}

type spyRecorder struct {
	mu    sync.Mutex
	input []messages.Message
}

func newSpy(inner language.ChatModel) *spyModel {
	return &spyModel{inner: inner, rec: &spyRecorder{}}
}

func (s *spyModel) record(in []messages.Message) {
	s.rec.mu.Lock()
	s.rec.input = append([]messages.Message(nil), in...)
	s.rec.mu.Unlock()
}

func (s *spyModel) snapshot() []messages.Message {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.rec.input
}

func (s *spyModel) Invoke(ctx context.Context, in []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	s.record(in)
	return s.inner.Invoke(ctx, in, opts...)
}

func (s *spyModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	s.record(in)
	return s.inner.Stream(ctx, in, opts...)
}

func (s *spyModel) Batch(ctx context.Context, in [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return s.inner.Batch(ctx, in, opts...)
}

func (s *spyModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	b, err := s.inner.BindTools(bound)
	if err != nil {
		return nil, err
	}
	return &spyModel{inner: b, rec: s.rec}, nil
}

func (s *spyModel) Capabilities() language.ChatModelCapabilities { return s.inner.Capabilities() }

func (s *spyModel) InputSchema() schema.Schema  { return s.inner.InputSchema() }
func (s *spyModel) OutputSchema() schema.Schema { return s.inner.OutputSchema() }

func TestTaskForkInheritsConversation(t *testing.T) {
	parentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "f1", Name: "Task", Args: map[string]any{
					"description": "continue in parallel",
					"prompt":      "finish the analysis",
					"fork":        true,
				}},
			}},
			messages.AI("parent done"),
		),
	)
	subSpy := newSpy(language.NewFakeChatModel(language.WithResponses(messages.AI("forked report"))))

	a, err := New(Config{
		Model:          parentModel,
		SubagentModel:  subSpy,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "parent-system",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	prior := []messages.Message{
		messages.Human("earlier question about the parser"),
		messages.AI("earlier answer: use recursion"),
	}
	out, err := a.Invoke(context.Background(), append(prior, messages.Human("please continue this in a fork")))
	if err != nil {
		t.Fatal(err)
	}

	// The subagent saw the parent's conversation plus the fork directive —
	// not just the bare prompt.
	got := subSpy.snapshot()
	if len(got) == 0 {
		t.Fatal("subagent was never invoked")
	}
	var sawEarlier, sawFork, sawParentSystem bool
	for _, m := range got {
		if strings.Contains(m.Content, "earlier answer: use recursion") {
			sawEarlier = true
		}
		if strings.Contains(m.Content, "<task-fork>") && strings.Contains(m.Content, "finish the analysis") {
			sawFork = true
		}
		if strings.Contains(m.Content, "parent-system") {
			sawParentSystem = true
		}
	}
	if !sawEarlier || !sawFork {
		t.Fatalf("fork history missing pieces: earlier=%v fork=%v (%d messages)", sawEarlier, sawFork, len(got))
	}
	if sawParentSystem {
		t.Fatal("parent system prompt must not leak into the forked history")
	}

	// The parent got the forked subagent's report back as the tool result.
	var report bool
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "forked report") {
			report = true
		}
	}
	if !report {
		t.Fatalf("forked report missing from transcript: %+v", out)
	}
}

func TestTaskWithoutForkBlankSlate(t *testing.T) {
	parentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "n1", Name: "Task", Args: map[string]any{"description": "fresh task", "prompt": "do it fresh"}},
			}},
			messages.AI("done"),
		),
	)
	subSpy := newSpy(language.NewFakeChatModel(language.WithResponses(messages.AI("fresh report"))))
	a, err := New(Config{
		Model:          parentModel,
		SubagentModel:  subSpy,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := []messages.Message{
		messages.Human("earlier question about the parser"),
		messages.AI("earlier answer: use recursion"),
	}
	if _, err := a.Invoke(context.Background(), append(prior, messages.Human("spawn a helper"))); err != nil {
		t.Fatal(err)
	}
	got := subSpy.snapshot()
	for _, m := range got {
		if strings.Contains(m.Content, "earlier answer") {
			t.Fatal("non-fork Task must not inherit the conversation")
		}
	}
	if len(got) != 2 { // subagent system prompt + the task prompt
		t.Fatalf("subagent input = %d messages, want 2: %+v", len(got), got)
	}
}
