package console

import (
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/agent"
)

// goalCmd implements /goal: the user-facing control surface of goal mode
// (agent.SetGoal/PauseGoal/ResumeGoal/ClearGoal). Starting the loop itself is
// the harness's job — the TUI watches for an active idle goal after every
// command and after each run, the web server starts a goal run from its goal
// API.
func (c *Console) goalCmd(args []string) (Result, error) {
	cmd, err := agent.ParseGoalCommand(args)
	if err != nil {
		return Result{}, err
	}
	switch cmd.Verb {
	case "help":
		return Result{Output: agent.GoalHelpText + "\n"}, nil

	case "status":
		g := c.deps.Agent.Goal()
		if g == nil {
			return Result{Output: "no goal set (use /goal set <objective>)\n"}, nil
		}
		return Result{Output: formatGoal(g)}, nil

	case "set":
		if g := c.deps.Agent.Goal(); g != nil && c.deps.Agent.GoalRunning() {
			return Result{}, fmt.Errorf("a goal loop is running (pause or clear it first)")
		}
		if _, err := c.deps.Agent.SetGoal(cmd.Objective, "", cmd.MaxCycles, cmd.MaxTokens); err != nil {
			return Result{}, err
		}
		g := c.deps.Agent.Goal()
		out := fmt.Sprintf("goal set (max %d cycles · %s tokens): %s\nthe loop starts now — completion claims are verified by an independent evaluator.\n",
			g.MaxCycles, agent.FormatGoalTokens(g.MaxTokens), g.Objective)
		return Result{Output: out}, nil

	case "pause":
		if !c.deps.Agent.PauseGoal() {
			g := c.deps.Agent.Goal()
			if g == nil {
				return Result{}, fmt.Errorf("no goal set")
			}
			return Result{}, fmt.Errorf("goal is %s, nothing to pause", g.Status)
		}
		return Result{Output: "goal paused (stops at the next cycle boundary; /goal resume to continue)\n"}, nil

	case "resume":
		if g := c.deps.Agent.Goal(); g == nil {
			return Result{}, fmt.Errorf("no goal set")
		}
		if !c.deps.Agent.ResumeGoal() {
			g := c.deps.Agent.Goal()
			if g.Status == agent.GoalActive {
				return Result{Output: "goal already active\n"}, nil
			}
			return Result{}, fmt.Errorf("cannot resume a %s goal", g.Status)
		}
		return Result{Output: "goal resumed — the loop continues at the next cycle\n"}, nil

	case "clear":
		if !c.deps.Agent.ClearGoal() {
			return Result{Output: "no goal set\n"}, nil
		}
		return Result{Output: "goal cleared\n"}, nil

	case "budget":
		var cycles int
		var tokens int64
		if cmd.BudgetKey == "cycles" {
			cycles = int(cmd.BudgetVal)
		} else {
			tokens = cmd.BudgetVal
		}
		if !c.deps.Agent.GoalSetBudget(cycles, tokens) {
			return Result{}, fmt.Errorf("no goal set")
		}
		g := c.deps.Agent.Goal()
		return Result{Output: fmt.Sprintf("budget: cycle %d/%d · %s/%s tokens\n", g.Cycles, g.MaxCycles, agent.FormatGoalTokens(g.TokensUsed), agent.FormatGoalTokens(g.MaxTokens))}, nil
	}
	return Result{}, fmt.Errorf("unknown /goal verb %q", cmd.Verb)
}

// formatGoal renders the goal status summary.
func formatGoal(g *agent.Goal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "goal [%s]: %s\n", g.Status, g.Objective)
	fmt.Fprintf(&b, "  cycle %d/%d · %s/%s tokens", g.Cycles, g.MaxCycles, agent.FormatGoalTokens(g.TokensUsed), agent.FormatGoalTokens(g.MaxTokens))
	if g.Claimed {
		b.WriteString(" · completion claim awaiting verification")
	}
	b.WriteString("\n")
	switch g.Status {
	case agent.GoalBlocked:
		fmt.Fprintf(&b, "  blocked: %s\n", g.BlockedReason)
	case agent.GoalComplete:
		fmt.Fprintf(&b, "  evidence: %s\n", g.Evidence)
	case agent.GoalBudgetLimited:
		fmt.Fprintf(&b, "  %s (adjust with /goal budget, then /goal resume)\n", g.BlockedReason)
	case agent.GoalPaused:
		b.WriteString("  paused — /goal resume to continue\n")
	}
	if len(g.Notes) > 0 {
		b.WriteString("  latest executor note: " + g.Notes[len(g.Notes)-1] + "\n")
	}
	if len(g.Progress) > 0 {
		b.WriteString("  cycles:\n")
		start := max(len(g.Progress)-5, 0)
		for _, p := range g.Progress[start:] {
			summary := p.Summary
			if summary == "" {
				summary = p.Note
			}
			fmt.Fprintf(&b, "    %2d [%s] %s\n", p.Cycle, p.Verdict, summary)
		}
	}
	return b.String()
}
