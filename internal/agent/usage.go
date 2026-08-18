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

// LastUsage is the provider-reported usage of the most recent model call:
// InputTokens + CacheReadTokens + CacheWriteTokens is that request's real
// context size (per the Messages API semantics), which the /context UI shows
// as the exact counterpart of the estimated breakdown.
type LastUsage struct {
	InputTokens     int `json:"input_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	OutputTokens    int `json:"output_tokens"`
}

// usageTracker is the thread-safe accumulator behind /cost.
type usageTracker struct {
	mu     sync.Mutex
	totals UsageTotals
	last   LastUsage
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
	lu := LastUsage{
		InputTokens:  um.InputTokens,
		OutputTokens: um.OutputTokens,
	}
	if um.InputTokenDetails != nil {
		lu.CacheReadTokens = um.InputTokenDetails.CacheReadInputTokens
		lu.CacheWriteTokens = um.InputTokenDetails.CacheCreationInputTokens
		u.totals.CacheReadTokens += um.InputTokenDetails.CacheReadInputTokens
		u.totals.CacheWriteTokens += um.InputTokenDetails.CacheCreationInputTokens
	}
	u.last = lu
}

// snapshot returns the current totals.
func (u *usageTracker) snapshot() UsageTotals {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.totals
}

// lastSnapshot returns the most recent model call's usage.
func (u *usageTracker) lastSnapshot() LastUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// Usage returns the accumulated session token usage.
func (a *Agent) Usage() UsageTotals { return a.usage.snapshot() }

// LastUsage returns the provider-reported usage of the most recent model
// call (zero value before the first call). It backs the context display's
// "exact" total.
func (a *Agent) LastUsage() LastUsage { return a.usage.lastSnapshot() }
