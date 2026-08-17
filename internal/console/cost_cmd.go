package console

import (
	"fmt"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/cost"
)

// cost renders the cumulative session usage and estimated cost, backed by
// the agent's usage tracker instead of history scans, so totals survive
// compaction and /clear.
func (c *Console) cost() string {
	u := c.deps.Agent.Usage()
	if u.Requests == 0 {
		return "no model usage recorded yet (stats appear after the first run)\n"
	}
	model := c.deps.Cfg.Model
	var b string
	b += fmt.Sprintf("session usage (%s):\n", model)
	b += fmt.Sprintf("  requests: %d\n", u.Requests)
	b += fmt.Sprintf("  input:    %d tokens", u.InputTokens)
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		b += fmt.Sprintf(" (cache read %d · cache write %d)", u.CacheReadTokens, u.CacheWriteTokens)
	}
	b += "\n"
	b += fmt.Sprintf("  output:   %d tokens\n", u.OutputTokens)
	b += fmt.Sprintf("  total:    %d tokens\n", u.TotalTokens)
	if est, known := cost.Estimate(model, usageFromAgent(u)); known {
		b += fmt.Sprintf("  estimated cost: %s\n", cost.FormatUSD(est))
	} else {
		b += "  estimated cost: unknown (no pricing entry for this model)\n"
	}
	return b
}

func usageFromAgent(u agent.UsageTotals) cost.Usage {
	return cost.Usage{
		Requests:         u.Requests,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}
