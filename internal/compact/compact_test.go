package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("hello world this is a test"),
		messages.AI("response"),
	}
	if got := EstimateTokens(msgs); got <= 0 {
		t.Fatalf("estimate = %d", got)
	}
}

func TestCompactBelowThresholdNoop(t *testing.T) {
	msgs := []messages.Message{messages.Human("hi"), messages.AI("hello")}
	out, err := Compact(context.Background(), msgs, 1000, func(ctx context.Context, msgs []messages.Message) (string, error) {
		t.Fatal("summarizer should not be called")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected unchanged, got %d messages", len(out))
	}
}

func TestCompactSummarizesHead(t *testing.T) {
	var msgs []messages.Message
	msgs = append(msgs, messages.System("system prompt"))
	for i := 0; i < 50; i++ {
		msgs = append(msgs, messages.Human("old message number "+strings.Repeat("x", 100)))
		msgs = append(msgs, messages.AI("old reply "+strings.Repeat("y", 100)))
	}
	// A short recent tail that we want to keep verbatim.
	msgs = append(msgs, messages.Human("recent question"), messages.AI("recent answer"))

	called := false
	out, err := Compact(context.Background(), msgs, 100,
		func(ctx context.Context, head []messages.Message) (string, error) {
			called = true
			if head[0].Role != messages.RoleHuman {
				t.Fatalf("head should start with old messages, got %v", head[0].Role)
			}
			return "summarized head", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("summarizer should have been called")
	}
	if out[0].Role != messages.RoleSystem || out[0].Content != "system prompt" {
		t.Fatalf("first message should preserve system prompt: %+v", out[0])
	}
	if out[1].Role != messages.RoleSystem || !strings.Contains(out[1].Content, "summarized head") {
		t.Fatalf("second message should be the summary: %+v", out[1])
	}
	if out[len(out)-1].Content != "recent answer" {
		t.Fatalf("tail should be preserved: %+v", out[len(out)-1])
	}
}

func TestMicrocompactTruncatesOldToolResults(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("go"),
		messages.Tool("1", strings.Repeat("x", 500)), // old, large
		messages.Tool("2", strings.Repeat("y", 500)), // old, large
		messages.Tool("3", "small recent"),           // recent, kept
	}
	out := Microcompact(msgs, 1, 200)
	if out[1].Content != "[truncated tool result]" {
		t.Fatalf("old tool result not truncated: %q", out[1].Content)
	}
	if out[2].Content != "[truncated tool result]" {
		t.Fatalf("old tool result not truncated: %q", out[2].Content)
	}
	if out[3].Content != "small recent" {
		t.Fatalf("recent tool result should be kept: %q", out[3].Content)
	}
	// Inputs not mutated.
	if msgs[1].Content == "[truncated tool result]" {
		t.Fatal("input was mutated")
	}
}

func TestSnipKeepsNewestWithinBudget(t *testing.T) {
	msgs := []messages.Message{
		messages.Tool("1", strings.Repeat("a", 100)), // old, exceeds budget
		messages.Tool("2", "newest"),                 // newest, kept
		messages.Human("after"),
	}
	out := Snip(msgs, 10)
	if out[0].Content != "[omitted tool result]" {
		t.Fatalf("old tool result should be omitted: %q", out[0].Content)
	}
	if out[1].Content != "newest" {
		t.Fatalf("newest tool result should be kept: %q", out[1].Content)
	}
	if out[2].Role != messages.RoleHuman {
		t.Fatalf("non-tool message should be untouched: %+v", out[2])
	}
}
