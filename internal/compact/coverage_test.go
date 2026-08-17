package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestCompactNilWriterAndSmallInput(t *testing.T) {
	if _, err := Compact(context.Background(), nil, 10, nil); err == nil {
		t.Fatal("expected nil-writer error")
	}
	msgs := []messages.Message{messages.Human("hi")}
	got, err := Compact(context.Background(), msgs, 100, func(ctx context.Context, msgs []messages.Message) (string, error) {
		return "earlier conversation summarized", nil
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("small input = %+v %v", got, err)
	}
	// Empty input passes through.
	got, err = Compact(context.Background(), nil, 0, func(ctx context.Context, msgs []messages.Message) (string, error) {
		return "x", nil
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty input = %+v %v", got, err)
	}
}

func TestCompactSummarizesOldMessages(t *testing.T) {
	// Enough messages to exceed a tiny budget.
	var msgs []messages.Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, messages.Human(strings.Repeat("word ", 20)))
	}
	out, err := Compact(context.Background(), msgs, 10, func(ctx context.Context, msgs []messages.Message) (string, error) {
		return "earlier conversation summarized", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("no compaction: %d messages", len(out))
	}
	if !strings.Contains(out[0].Content, "summarized") {
		t.Fatalf("head = %q", out[0].Content)
	}
	// A failing summarizer surfaces the error.
	if _, err := Compact(context.Background(), msgs, 10, func(ctx context.Context, msgs []messages.Message) (string, error) {
		return "", context.Canceled
	}); err == nil {
		t.Fatal("expected summarizer error")
	}
}

func TestEstimateMessageShapes(t *testing.T) {
	// Tool calls contribute their names and serialized args; empty messages
	// still count as one token.
	m := messages.Message{
		Role: messages.RoleAI,
		ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Read", Args: map[string]any{"file_path": "x.go"}},
		},
	}
	if got := estimateMessage(m); got <= 1 {
		t.Fatalf("tool-call estimate = %d", got)
	}
	if got := estimateMessage(messages.Message{}); got != 1 {
		t.Fatalf("empty estimate = %d", got)
	}
}
