package console

// Runtime agent controls shared by the TUI and the line REPL: /thinking
// (extended-thinking level) and /quota (provider plan quota). The same
// operations are exposed to the web UI via /api/agent and /api/quota.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/quota"
)

// liveCfg returns the agent's live configuration, preferring the CfgLive
// snapshot (which reflects changes made from the web UI) over the console's
// construction-time copy.
func (c *Console) liveCfg() config.Config {
	if c.deps.CfgLive != nil {
		return c.deps.CfgLive()
	}
	return *c.deps.Cfg
}

// thinkingCmd implements /thinking: no args shows the current level; an arg
// (off | low | medium | high | <tokens>) rebuilds the model with that budget.
func (c *Console) thinkingCmd(args []string) (Result, error) {
	budget := c.liveCfg().ThinkingBudget
	if len(args) == 0 {
		return Result{Output: fmt.Sprintf("current thinking=%s (%d tokens; use /thinking <off|low|medium|high|tokens>)\n",
			config.ThinkingLevel(budget), budget)}, nil
	}

	budget, err := parseThinkingBudget(args[0])
	if err != nil {
		return Result{}, err
	}
	// The provider template gates thinking: models without a thinking mode
	// reject the switch regardless of provider.
	cfg := c.liveCfg()
	if budget > 0 {
		levels := config.ThinkingLevelsFor(cfg.Provider, cfg.Model)
		if !containsLevel(levels, config.ThinkingLevel(budget)) {
			return Result{}, fmt.Errorf("model %s (%s) does not support thinking (levels %v)",
				cfg.Model, cfg.Provider, levels)
		}
	}
	if c.deps.SetThinkingBudget == nil {
		return Result{}, fmt.Errorf("thinking switching is not available in this session")
	}
	if err := c.deps.SetThinkingBudget(budget); err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("thinking set to %s (%d tokens) — applies from the next turn\n",
		config.ThinkingLevel(budget), budget)}, nil
}

func containsLevel(levels []string, level string) bool {
	return slices.Contains(levels, level)
}

// parseThinkingBudget resolves a level name or raw token count to a budget.
func parseThinkingBudget(arg string) (int, error) {
	if b, ok := config.ThinkingBudgetFor(arg); ok {
		return b, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 || (n != 0 && (n < config.MinThinkingBudget || n > config.MaxThinkingBudget)) {
		return 0, fmt.Errorf("invalid thinking level %q (off | low | medium | high | token count in [1024, 31999])", arg)
	}
	return n, nil
}

// quotaCmd implements /quota: the provider plan's remaining allowance (GLM
// Coding Plan), falling back to a pointer at /cost when unavailable.
func (c *Console) quotaCmd(ctx context.Context) (Result, error) {
	if c.deps.Quota == nil {
		return Result{Output: "quota query is not available in this session\n"}, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := c.deps.Quota(qctx)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: quota.Format(info)}, nil
}
