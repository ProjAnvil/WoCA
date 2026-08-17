package agent

import (
	"context"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/compact"
)

// Reactive compaction (prompt-too-long recovery): when a model call
// fails because the prompt exceeds the provider's context window, the
// conversation is compacted (summarized) and the turn is retried once with
// the compacted history. A single-attempt guard ensures a second
// prompt-too-long failure surfaces the error instead of looping.

// promptTooLongPatterns matches the provider error strings for an oversized
// prompt: Anthropic "prompt is too long: N tokens > M maximum", OpenAI
// "maximum context length is N tokens" / code "context_length_exceeded",
// Vertex "exceeds the maximum number of tokens".
var promptTooLongPatterns = []string{
	"prompt is too long",
	"prompt too long",
	"maximum context length",
	"context_length_exceeded",
	"exceeds the maximum number of tokens",
	"input length and max_tokens exceed",
}

// IsPromptTooLongError reports whether err is a context-window-exceeded
// rejection from the provider.
func IsPromptTooLongError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, p := range promptTooLongPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// ReactiveCompactHistory compacts the conversation for the prompt-too-long
// recovery path. Unlike auto-compaction it ignores the configured estimate
// threshold — the provider already rejected the request — and targets the
// configured compaction budget, or a quarter of the current estimate when
// no budget is set.
func (a *Agent) ReactiveCompactHistory(ctx context.Context, history []messages.Message) []messages.Message {
	if a.summarizer == nil || len(history) == 0 {
		return history
	}
	if h := a.hooksFor(ctx); h != nil {
		if _, blocked, err := h.PreCompact(ctx); err == nil && blocked {
			return history
		}
	}
	budget := a.compactLimit
	if budget <= 0 {
		budget = compact.EstimateTokens(history) / 4
	}
	if budget < 1 {
		budget = 1
	}
	history = compact.Snip(history, budget*4)
	history = compact.Microcompact(history, 10, 2000)
	out, err := compact.Compact(ctx, history, budget, SummarizeWithModel(a.summarizer))
	if err != nil {
		return history // summarization failed; the caller surfaces the original error
	}
	return out
}

// CompactionShrank reports whether compaction produced a meaningfully
// smaller history (by token estimate or message count). A no-op compaction
// must not trigger a pointless retry.
func CompactionShrank(before, after []messages.Message) bool {
	if len(after) < len(before) {
		return true
	}
	eb := compact.EstimateTokens(before)
	ea := compact.EstimateTokens(after)
	return ea < eb*9/10 // ≥10% smaller
}
