// Package cost estimates session cost from accumulated token usage and a
// model pricing table. Prices are USD per million tokens; estimates are
// informational, not billing.
package cost

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Pricing is a per-million-token price pair.
type Pricing struct {
	Input  float64
	Output float64
	// CacheReadFactor and CacheWriteFactor scale the input price for cached
	// tokens (Anthropic: 0.1 reads, 1.25 writes). Zero means cache tokens are
	// priced like regular input.
	CacheReadFactor  float64
	CacheWriteFactor float64
}

// Table maps lowercase model-substring patterns to pricing; the first matching
// pattern (in order) wins. Prefix-agnostic so provider deployments and dated
// snapshots ("claude-sonnet-4-20250514", "openai/gpt-4o-2024-08-06") match.
var Table = []struct {
	Pattern  *regexp.Regexp
	Pricing  Pricing
	Fullname string
}{
	{regexp.MustCompile(`claude.*opus`), Pricing{15, 75, 0.1, 1.25}, "Claude Opus"},
	{regexp.MustCompile(`claude.*sonnet`), Pricing{3, 15, 0.1, 1.25}, "Claude Sonnet"},
	{regexp.MustCompile(`claude.*haiku`), Pricing{0.8, 4, 0.1, 1.25}, "Claude Haiku"},
	{regexp.MustCompile(`gpt-5(\.|-|$)`), Pricing{1.25, 10, 0, 0}, "GPT-5"},
	{regexp.MustCompile(`gpt-4\.1`), Pricing{2, 8, 0, 0}, "GPT-4.1"},
	{regexp.MustCompile(`gpt-4o-mini`), Pricing{0.15, 0.6, 0, 0}, "GPT-4o mini"},
	{regexp.MustCompile(`gpt-4o`), Pricing{2.5, 10, 0, 0}, "GPT-4o"},
	{regexp.MustCompile(`o3`), Pricing{2, 8, 0, 0}, "o3"},
	{regexp.MustCompile(`o4-mini`), Pricing{1.1, 4.4, 0, 0}, "o4-mini"},
	{regexp.MustCompile(`glm-4(\.|-|$)`), Pricing{0.6, 2.2, 0, 0}, "GLM-4"},
	{regexp.MustCompile(`glm-5`), Pricing{0.8, 3, 0, 0}, "GLM-5"},
	{regexp.MustCompile(`deepseek`), Pricing{0.27, 1.1, 0, 0}, "DeepSeek"},
}

// Lookup finds the pricing entry for a model name (case-insensitive substring
// match, most specific patterns first).
func Lookup(model string) (Pricing, string, bool) {
	m := strings.ToLower(model)
	for _, e := range Table {
		if e.Pattern.MatchString(m) {
			return e.Pricing, e.Fullname, true
		}
	}
	return Pricing{}, "", false
}

// Usage is the token accounting input to Estimate (mirrors agent.UsageTotals
// without the agent dependency).
type Usage struct {
	Requests         int
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Estimate computes the estimated session cost in USD. Cache factors apply
// only when the model's pricing defines them.
func Estimate(model string, u Usage) (cost float64, known bool) {
	p, _, known := Lookup(model)
	if !known {
		return 0, false
	}
	fresh := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if fresh < 0 {
		fresh = 0
	}
	cost = float64(fresh) / 1e6 * p.Input
	cost += float64(u.OutputTokens) / 1e6 * p.Output
	if p.CacheReadFactor > 0 {
		cost += float64(u.CacheReadTokens) / 1e6 * p.Input * p.CacheReadFactor
	} else {
		cost += float64(u.CacheReadTokens) / 1e6 * p.Input
	}
	if p.CacheWriteFactor > 0 {
		cost += float64(u.CacheWriteTokens) / 1e6 * p.Input * p.CacheWriteFactor
	} else {
		cost += float64(u.CacheWriteTokens) / 1e6 * p.Input
	}
	return cost, true
}

// FormatUSD renders a cost with two significant decimals, rounding display
// noise away.
func FormatUSD(v float64) string {
	if v == 0 {
		return "$0.00"
	}
	if v < 0.01 {
		return fmt.Sprintf("$%.4f", math.Round(v*10000)/10000)
	}
	return fmt.Sprintf("$%.2f", v)
}
