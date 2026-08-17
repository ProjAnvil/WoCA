// Package compact implements auto-compaction: when a conversation
// approaches the context budget, older messages are summarized and replaced by
// a summary message, keeping the recent tail intact.
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tokens"
)

// SummaryWriter summarizes a batch of messages into prose. Injectable for
// tests; in production it uses a cheap model.
type SummaryWriter func(ctx context.Context, msgs []messages.Message) (string, error)

// EstimateTokens approximates the token count of a message list using the
// word/punctuation/CJK heuristic from the tokens package.
func EstimateTokens(msgs []messages.Message) int {
	return tokens.Estimate(msgs)
}

func estimateMessage(m messages.Message) int {
	n := tokens.EstimateText(m.Content)
	for _, tc := range m.ToolCalls {
		n += tokens.EstimateText(tc.Name)
		if raw, err := json.Marshal(tc.Args); err == nil {
			n += tokens.EstimateText(string(raw))
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

// Compact summarizes the older messages when the list exceeds maxTokens,
// preserving the leading system message and the recent tail. When no
// compaction is needed, the input is returned unchanged.
func Compact(ctx context.Context, msgs []messages.Message, maxTokens int, write SummaryWriter) ([]messages.Message, error) {
	if write == nil {
		return msgs, fmt.Errorf("compact: SummaryWriter is required")
	}
	if len(msgs) == 0 || EstimateTokens(msgs) <= maxTokens {
		return msgs, nil
	}

	// Keep the recent tail within half the budget; always keep the last message.
	keep := maxTokens / 2
	if keep < 1 {
		keep = 1
	}
	cut := len(msgs)
	used := estimateMessage(msgs[len(msgs)-1])
	for i := len(msgs) - 2; i >= 0; i-- {
		t := estimateMessage(msgs[i])
		if used+t > keep {
			break
		}
		used += t
		cut = i
	}
	// Always keep the last message, even when it alone exceeds the budget —
	// summarizing it away would discard the user's current prompt.
	if cut > len(msgs)-1 {
		cut = len(msgs) - 1
	}
	if cut <= 0 {
		return msgs, nil
	}

	head := msgs[:cut]
	tail := msgs[cut:]

	// Preserve a leading system message (the system prompt) at the front.
	var prefix []messages.Message
	if len(head) > 0 && head[0].Role == messages.RoleSystem {
		prefix = []messages.Message{head[0]}
		head = head[1:]
	}
	if len(head) == 0 {
		return msgs, nil
	}

	summary, err := write(ctx, head)
	if err != nil {
		return msgs, err
	}

	out := make([]messages.Message, 0, len(prefix)+1+len(tail))
	out = append(out, prefix...)
	out = append(out, messages.System(summaryPrefix(strings.TrimSpace(summary))))
	out = append(out, tail...)
	return out, nil
}

func summaryPrefix(summary string) string {
	return "<summary of earlier conversation>\n" + summary
}

// Microcompact replaces tool-result messages older than the most recent
// keepRecent with a placeholder once their content exceeds maxChars. It never
// touches non-tool messages and returns a new slice (inputs are not mutated).
func Microcompact(msgs []messages.Message, keepRecent, maxChars int) []messages.Message {
	out := append([]messages.Message(nil), msgs...)
	var toolIdx []int
	for i, m := range out {
		if m.Role == messages.RoleTool {
			toolIdx = append(toolIdx, i)
		}
	}
	skip := len(toolIdx) - keepRecent
	if skip < 0 {
		skip = 0
	}
	for i := 0; i < skip; i++ {
		idx := toolIdx[i]
		if len(out[idx].Content) > maxChars {
			out[idx].Content = "[truncated tool result]"
			out[idx].ContentBlocks = nil
		}
	}
	return out
}

// Snip trims the total character budget of tool-result messages, keeping the
// newest ones and replacing older ones with a placeholder. It never drops
// non-tool messages and returns a new slice.
func Snip(msgs []messages.Message, maxChars int) []messages.Message {
	out := append([]messages.Message(nil), msgs...)
	used := 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != messages.RoleTool {
			continue
		}
		if used+len(out[i].Content) > maxChars {
			out[i].Content = "[omitted tool result]"
			out[i].ContentBlocks = nil
			continue
		}
		used += len(out[i].Content)
	}
	return out
}
