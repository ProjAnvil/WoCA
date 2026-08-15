package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// The goal model tools — a restricted view of the goal state machine,
// mirroring Codex's deliberate split: the model may read the goal, report
// progress, flag itself blocked, and claim completion (subject to skeptical
// verification), but it can never pause, resume, clear, or replace the
// objective. Those are user-only actions (/goal, the web goal API).

const getGoalDescription = `Inspect the active goal: objective, status, cycle and token budgets, per-cycle progress, and any pending completion claim. Returns "(no active goal)" when the user has not set one. Read-only.`

const updateGoalDescription = `Report on the active goal from inside a goal cycle. Actions:
- "progress": record a short note about what was done this cycle (note).
- "claim_complete": claim the goal is fully achieved, with concrete evidence (evidence). An independent skeptical evaluator verifies the claim against tool results; an unverified claim continues the loop.
- "blocked": flag a hard blocker you cannot resolve yourself, with the reason (reason). The goal stops and waits for the user.

You cannot pause, resume, clear, or change the objective — only the user can.`

type updateGoalArgs struct {
	Action   string `json:"action"`
	Note     string `json:"note,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// goalTools builds the GetGoal and UpdateGoal tools.
func (a *Agent) goalTools() []coretools.Tool {
	getTool, err := coretools.FromFunc("GetGoal", getGoalDescription,
		func(_ context.Context, _ struct{}) (coretools.Result, error) {
			g := a.Goal()
			if g == nil {
				return coretools.Result{Content: "(no active goal)"}, nil
			}
			data, err := json.MarshalIndent(g, "", "  ")
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			return coretools.Result{Content: string(data)}, nil
		})
	if err != nil {
		panic(err)
	}
	updateTool, err := coretools.FromFunc("UpdateGoal", updateGoalDescription,
		func(_ context.Context, args updateGoalArgs) (coretools.Result, error) {
			if a.Goal() == nil {
				return coretools.Result{Content: "Error: no active goal"}, nil
			}
			switch strings.ToLower(strings.TrimSpace(args.Action)) {
			case "progress":
				if strings.TrimSpace(args.Note) == "" {
					return coretools.Result{Content: "Error: progress requires a note"}, nil
				}
				a.goalAddNote(args.Note)
				return coretools.Result{Content: "Progress noted. Keep going, or claim_complete when the goal is verifiably achieved."}, nil
			case "claim_complete":
				if strings.TrimSpace(args.Evidence) == "" {
					return coretools.Result{Content: "Error: claim_complete requires evidence (what you did and how it can be verified)"}, nil
				}
				if !a.goalClaimCompletion(args.Evidence) {
					return coretools.Result{Content: "Error: no active goal"}, nil
				}
				return coretools.Result{Content: "Completion claim recorded. An independent evaluator will verify the evidence; finish the cycle normally."}, nil
			case "blocked":
				reason := orString(args.Reason, args.Note)
				if strings.TrimSpace(reason) == "" {
					return coretools.Result{Content: "Error: blocked requires a reason"}, nil
				}
				if !a.goalMarkBlocked(reason) {
					return coretools.Result{Content: "Error: goal is not active"}, nil
				}
				return coretools.Result{Content: fmt.Sprintf("Goal marked blocked: %s. Finish the current tool calls; the loop will stop and wait for the user.", reason)}, nil
			}
			return coretools.Result{Content: "Error: unknown action (use progress | claim_complete | blocked)"}, nil
		})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{getTool, updateTool}
}
