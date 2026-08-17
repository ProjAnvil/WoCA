package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func TestIsPromptTooLongError(t *testing.T) {
	pos := []string{
		"prompt is too long: 137500 tokens > 135000 maximum",
		"Prompt Too Long", // case-insensitive
		"this model's maximum context length is 8192 tokens. however, you requested 9000 tokens",
		"code: context_length_exceeded",
		"the request exceeds the maximum number of tokens allowed",
		"input length and max_tokens exceed context limit",
	}
	for _, s := range pos {
		if !IsPromptTooLongError(errors.New(s)) {
			t.Fatalf("missed prompt-too-long error: %q", s)
		}
	}
	neg := []string{
		"529 overloaded_error",
		"invalid api key",
		"connection reset by peer",
		"context canceled",
	}
	for _, s := range neg {
		if IsPromptTooLongError(errors.New(s)) {
			t.Fatalf("false positive on %q", s)
		}
	}
	if IsPromptTooLongError(nil) {
		t.Fatal("nil error misdetected")
	}
}

// longHistory builds a multi-message history large enough for compaction to
// find a cut point.
func longHistory() []messages.Message {
	out := []messages.Message{messages.Human("start the task")}
	for i := 0; i < 8; i++ {
		out = append(out,
			messages.AI(strings.Repeat("assistant analysis number ", 40)+string(rune('a'+i))),
			messages.Human(strings.Repeat("follow up question ", 40)+string(rune('A'+i))),
		)
	}
	return out
}

func textScript(s string) agentStreamScript {
	return agentStreamScript{failAfter: -1, chunks: []messages.Message{func() messages.Message {
		m := messages.AI("")
		m.Content = s
		return m
	}()}}
}

func TestInvokePromptTooLongRecovery(t *testing.T) {
	ptl := errors.New("prompt is too long: 137500 tokens > 135000 maximum")
	model := &scheduledModel{scripts: []agentStreamScript{
		{failAfter: 0, err: ptl},
		textScript("recovered after compaction"),
	}}
	a, err := New(Config{
		Model:          model,
		Summarizer:     model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), longHistory(), "t")
	if err != nil {
		t.Fatal(err)
	}

	// The turn was retried exactly once on the compacted history.
	if got := model.streamCalls(); got != 2 {
		t.Fatalf("model stream calls = %d, want 2 (failed turn + retried turn)", got)
	}
	var sawSummary bool
	for _, m := range out {
		if m.Role == messages.RoleSystem && strings.Contains(m.Content, "summary of the conversation") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("compacted transcript missing the summary message")
	}
	if last := out[len(out)-1]; last.Role != messages.RoleAI || last.Content != "recovered after compaction" {
		t.Fatalf("final message = %+v", last)
	}
	// The retried turn saw a shorter context than the original history.
	// (The summary replaced the compacted head.)
	if len(out) >= len(longHistory())+1 { // +1 for the system prompt prefix
		t.Fatalf("transcript not compacted: %d messages", len(out))
	}
}

func TestInvokePromptTooLongSingleRetryGuard(t *testing.T) {
	ptl := errors.New("prompt is too long: 200000 tokens > 135000 maximum")
	model := &scheduledModel{scripts: []agentStreamScript{
		{failAfter: 0, err: ptl},
		{failAfter: 0, err: ptl},
	}}
	a, err := New(Config{
		Model:          model,
		Summarizer:     model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(context.Background(), longHistory(), "t")
	if !IsPromptTooLongError(err) {
		t.Fatalf("expected the second prompt-too-long error to surface, got %v", err)
	}
	if got := model.streamCalls(); got != 2 {
		t.Fatalf("model stream calls = %d, want 2 (no retry loop past one attempt)", got)
	}
}

func TestInvokePromptTooLongNothingToCompact(t *testing.T) {
	ptl := errors.New("prompt is too long: 137500 tokens > 135000 maximum")
	model := &scheduledModel{scripts: []agentStreamScript{
		{failAfter: 0, err: ptl},
	}}
	a, err := New(Config{
		Model:          model,
		Summarizer:     model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A single huge human message: compaction has no cut point, so the
	// provider error surfaces without a pointless retry.
	huge := messages.Human(strings.Repeat("one enormous prompt ", 2000))
	_, err = a.Invoke(context.Background(), []messages.Message{huge}, "t")
	if !IsPromptTooLongError(err) {
		t.Fatalf("expected prompt-too-long to surface, got %v", err)
	}
	if got := model.streamCalls(); got != 1 {
		t.Fatalf("model stream calls = %d, want 1 (no retry when compaction cannot shrink)", got)
	}
}

func TestCompactionShrank(t *testing.T) {
	before := longHistory()
	after := []messages.Message{before[0], before[len(before)-1]}
	if !CompactionShrank(before, after) {
		t.Fatal("shrink not detected")
	}
	if CompactionShrank(before, before) {
		t.Fatal("identical history misdetected as shrunk")
	}
	if CompactionShrank(before, append(append([]messages.Message{}, before...), messages.Human("more"))) {
		t.Fatal("grown history misdetected as shrunk")
	}
}
