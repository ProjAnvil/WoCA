package agent

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Tool result size budget: the tool results of one tool round (a maximal run
// of consecutive tool messages) must fit an aggregate character budget. When
// a round exceeds it, the largest results are replaced by a preview
// placeholder until it fits — the full content is dropped, the model is told
// to re-run the tool narrower.
//
// The function is deterministic per round and idempotent, so re-applying it
// on every model request yields byte-identical messages (prompt-cache safe)
// and results the model already saw unreplaced are never retroactively
// replaced — "frozen" semantics fall out of determinism because a finished
// round's content never changes.

const (
	// DefaultToolResultBudgetChars is the aggregate budget per tool round.
	DefaultToolResultBudgetChars = 200_000
	// ToolResultPreviewChars is how much of a replaced result is kept.
	ToolResultPreviewChars = 2000
)

// toolResultPlaceholderPrefix marks already-replaced content (idempotency).
const toolResultPlaceholderPrefix = "[tool result replaced to fit the context budget"

// ApplyToolResultBudget enforces the aggregate budget on every run of
// consecutive tool-result messages. It returns msgs unchanged when nothing
// needs replacing; otherwise a new slice (the input is never mutated).
// budget <= 0 disables enforcement.
func ApplyToolResultBudget(msgs []messages.Message, budget int) []messages.Message {
	if budget <= 0 {
		return msgs
	}
	out := msgs
	for i := 0; i < len(msgs); {
		if msgs[i].Role != messages.RoleTool {
			i++
			continue
		}
		j := i
		for j < len(msgs) && msgs[j].Role == messages.RoleTool {
			j++
		}
		out = budgetRound(out, i, j, budget)
		i = j
	}
	return out
}

// budgetRound enforces the budget on msgs[from:to] (all tool messages),
// returning the updated slice.
func budgetRound(msgs []messages.Message, from, to, budget int) []messages.Message {
	total := 0
	type candidate struct {
		idx  int // absolute index in msgs
		size int
	}
	var candidates []candidate
	for k := from; k < to; k++ {
		n := len(msgs[k].Content)
		total += n
		// Only results large enough to be worth replacing, and never an
		// already-replaced one (idempotency).
		if n > ToolResultPreviewChars && !strings.HasPrefix(msgs[k].Content, toolResultPlaceholderPrefix) {
			candidates = append(candidates, candidate{idx: k, size: n})
		}
	}
	if total <= budget || len(candidates) == 0 {
		return msgs
	}

	// Replace the largest results first — cheapest path under budget.
	slices.SortFunc(candidates, func(a, b candidate) int { return cmp.Compare(b.size, a.size) })
	out := append([]messages.Message(nil), msgs...) // copy; input never mutated
	over := total - budget
	for _, c := range candidates {
		if over <= 0 {
			break
		}
		replacement := toolResultPlaceholder(out[c.idx].Content)
		over -= c.size - len(replacement)
		out[c.idx] = messages.Tool(out[c.idx].ToolCallID, replacement)
	}
	return out
}

// toolResultPlaceholder builds the deterministic replacement: what was
// removed, a preview, and how to get the content back.
func toolResultPlaceholder(content string) string {
	preview := truncateRunes(content, ToolResultPreviewChars)
	return fmt.Sprintf(
		"%s: %d characters removed, first %d follow]\n%s\n[preview end — re-run the tool with narrower parameters (offset/limit/pattern) to retrieve the exact portion]",
		toolResultPlaceholderPrefix, len(content), len(preview), preview)
}

// truncateRunes cuts s to at most max runes on a rune boundary.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
