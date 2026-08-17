package agent

import (
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
)

// UsageTotals accumulates provider-reported token usage across every model
// call of a session (the input of cost tracking). Unlike history-derived
// counts it survives compaction and /clear.
type UsageTotals struct {
	Requests         int `json:"requests"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// usageTracker is the thread-safe accumulator behind /cost.
type usageTracker struct {
	mu     sync.Mutex
	totals UsageTotals
}

// recordUsage folds one model response's usage into the totals. Zero-usage
// chunks (stream deltas) are skipped, so requests counts model calls, not
// chunks.
func (u *usageTracker) recordUsage(um messages.UsageMetadata) {
	if um.TotalTokens == 0 && um.InputTokens == 0 && um.OutputTokens == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.totals.Requests++
	u.totals.InputTokens += um.InputTokens
	u.totals.OutputTokens += um.OutputTokens
	u.totals.TotalTokens += um.TotalTokens
	if um.InputTokenDetails != nil {
		u.totals.CacheReadTokens += um.InputTokenDetails.CacheReadInputTokens
		u.totals.CacheWriteTokens += um.InputTokenDetails.CacheCreationInputTokens
	}
}

// snapshot returns the current totals.
func (u *usageTracker) snapshot() UsageTotals {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.totals
}

// Usage returns the accumulated session token usage.
func (a *Agent) Usage() UsageTotals { return a.usage.snapshot() }
