package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func TestSetModelSwapsRuntimeModel(t *testing.T) {
	first := language.NewFakeChatModel(language.WithResponses(messages.AI("from first")))
	a, err := New(Config{
		Model:          first,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	second := language.NewFakeChatModel(language.WithResponses(messages.AI("from second")))
	if err := a.SetModel(second); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatal(err)
	}
	last := out[len(out)-1]
	if last.Content != "from second" {
		t.Fatalf("after SetModel, reply = %q, want %q", last.Content, "from second")
	}

	if err := a.SetModel(nil); err == nil {
		t.Fatal("SetModel(nil) should error")
	}
}

func TestToolNamesAndSkills(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:        model,
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	names := a.ToolNames()
	if len(names) == 0 {
		t.Fatal("ToolNames() empty")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("ToolNames not sorted: %v", names)
		}
	}
	found := false
	for _, n := range names {
		if n == "Read" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ToolNames missing Read: %v", names)
	}
	if a.CompactThreshold() != 0 {
		t.Fatalf("CompactThreshold = %d, want 0 by default", a.CompactThreshold())
	}
	if len(a.Skills()) != 0 {
		t.Fatalf("Skills() = %v, want empty by default", a.Skills())
	}
}

// failingSummarizer is a chat model whose Invoke always errors, to trip the
// auto-compaction breaker.
type failingSummarizer struct {
	*language.FakeChatModel
}

func (failingSummarizer) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("summarizer boom")
}

func newCompactAgent(t *testing.T, summarizer language.ChatModel) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:            model,
		Registry:         tools.NewRegistry(t.TempDir()),
		SystemPrompt:     "test",
		PermissionMode:   config.ModeBypass,
		CompactThreshold: 10,
		Summarizer:       summarizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func bigHistory() []messages.Message {
	out := make([]messages.Message, 0, 8)
	for i := 0; i < 8; i++ {
		out = append(out, messages.Human("this is a fairly long line of text to push the token estimate up "+string(rune('a'+i))))
	}
	return out
}

func TestCompactBreakerTripsAndForces(t *testing.T) {
	a := newCompactAgent(t, failingSummarizer{language.NewFakeChatModel()})
	history := bigHistory()

	// Each failing auto-compact increments the failure count.
	for i := 1; i <= maxCompactFails; i++ {
		a.CompactHistory(context.Background(), history)
		if got := a.compactFails; got != i {
			t.Fatalf("compactFails = %d after %d failures, want %d", got, i, i)
		}
	}

	// Breaker tripped: auto-compaction is now a no-op even though the
	// summarizer would be invoked again.
	a.CompactHistory(context.Background(), history)
	if a.compactFails != maxCompactFails {
		t.Fatalf("compactFails = %d, breaker should hold at %d", a.compactFails, maxCompactFails)
	}
}

func TestForceCompactIgnoresBreaker(t *testing.T) {
	a := newCompactAgent(t, failingSummarizer{language.NewFakeChatModel()})
	history := bigHistory()
	for i := 0; i < maxCompactFails; i++ {
		a.CompactHistory(context.Background(), history)
	}

	// ForceCompact still attempts (and fails) without growing the counter.
	out := a.ForceCompact(context.Background(), history)
	if len(out) != len(history) {
		t.Fatalf("ForceCompact on failure should return history unchanged")
	}
	if a.compactFails != maxCompactFails {
		t.Fatalf("compactFails = %d after forced failure, want unchanged %d", a.compactFails, maxCompactFails)
	}
}

func TestCompactResetsBreakerOnSuccess(t *testing.T) {
	good := language.NewFakeChatModel(language.WithResponses(messages.AI("summary of the conversation")))
	a := newCompactAgent(t, failingSummarizer{language.NewFakeChatModel()})
	history := bigHistory()
	a.CompactHistory(context.Background(), history)
	a.CompactHistory(context.Background(), history)
	if a.compactFails != 2 {
		t.Fatalf("compactFails = %d, want 2", a.compactFails)
	}

	// Swap in a working summarizer; success resets the breaker.
	a2 := newCompactAgent(t, good)
	out := a2.CompactHistory(context.Background(), history)
	if len(out) >= len(history) {
		t.Fatalf("successful compaction should shrink history: %d -> %d", len(history), len(out))
	}
	if a2.compactFails != 0 {
		t.Fatalf("compactFails = %d after success, want 0", a2.compactFails)
	}
}
