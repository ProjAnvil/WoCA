package runner

import (
	"fmt"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tokens"
)

// Token budget auto-continue, a port of Claude Code's query/tokenBudget.ts +
// query.ts stage 22e: when a turn ends naturally but the configured output
// budget is not yet 90% spent, a nudge message is injected and the run
// continues ("500k auto-continue"). Continuation stops on diminishing
// returns — 3+ consecutive continuations whose token deltas are both under
// 500 — or when the budget is reached.

const (
	// DefaultTurnTokenBudget mirrors Claude Code's 500k auto-continue.
	DefaultTurnTokenBudget = 500_000
	// budgetCompletionThreshold is the fraction of the budget at which the
	// turn is considered complete (Claude Code's COMPLETION_THRESHOLD 0.9).
	budgetCompletionThreshold = 0.9
	// diminishingThreshold is the per-continuation token delta below which
	// progress counts as diminishing (Claude Code's 500).
	diminishingThreshold = 500
	// maxDiminishingContinuations is the continuation count from which the
	// diminishing check applies (Claude Code's >= 3).
	maxDiminishingContinuations = 3
)

// tokenBudgetTracker carries the auto-continue state across one run.
type tokenBudgetTracker struct {
	continuationCount int
	lastDeltaTokens   int
	lastTotalTokens   int
}

// tokenBudgetDecision is the outcome of one budget check.
type tokenBudgetDecision struct {
	// Continue requests a nudge continuation with the given message.
	Continue bool
	Nudge    string
	// Diminishing marks a stop caused by diminishing returns.
	Diminishing bool
}

// checkTokenBudget decides whether the turn should continue. cumulative is
// the turn's total output tokens so far (only tracked for the top-level
// run; subagents never auto-continue).
func checkTokenBudget(tr *tokenBudgetTracker, budget, cumulative int) tokenBudgetDecision {
	if budget <= 0 {
		return tokenBudgetDecision{}
	}
	delta := cumulative - tr.lastTotalTokens
	diminishing := tr.continuationCount >= maxDiminishingContinuations &&
		delta < diminishingThreshold &&
		tr.lastDeltaTokens < diminishingThreshold
	if !diminishing && float64(cumulative) < budgetCompletionThreshold*float64(budget) {
		tr.continuationCount++
		tr.lastDeltaTokens = delta
		tr.lastTotalTokens = cumulative
		pct := 0
		if budget > 0 {
			pct = cumulative * 100 / budget
		}
		return tokenBudgetDecision{
			Continue: true,
			Nudge:    budgetContinuationMessage(pct, cumulative, budget),
		}
	}
	return tokenBudgetDecision{Diminishing: diminishing}
}

// budgetContinuationMessage mirrors Claude Code's nudge text.
func budgetContinuationMessage(pct, turnTokens, budget int) string {
	return fmt.Sprintf("Stopped at %d%% of token target (%d / %d). Keep working — do not summarize.", pct, turnTokens, budget)
}

// estimateTurnOutputTokens approximates the model output of the messages
// produced this turn (AI text and tool calls; tool results are not model
// output).
func estimateTurnOutputTokens(msgs []messages.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != messages.RoleAI {
			continue
		}
		n += tokens.EstimateText(m.Content)
		for _, tc := range m.ToolCalls {
			n += tokens.EstimateText(tc.Name + " " + fmt.Sprintf("%v", tc.Args))
		}
	}
	return n
}
