package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/graph"
)

// goalSummary extracts the terminal narrative from a goal snapshot: the
// verified evidence for completions, the blocking reason for blocked and
// budget-limited goals, falling back to the latest executor note.
func goalSummary(g *agent.Goal) string {
	if s := strings.TrimSpace(g.Evidence); s != "" {
		return s
	}
	if s := strings.TrimSpace(g.BlockedReason); s != "" {
		return s
	}
	if len(g.Notes) > 0 {
		return g.Notes[len(g.Notes)-1]
	}
	if len(g.Progress) > 0 {
		return g.Progress[len(g.Progress)-1].Note
	}
	return ""
}

// RunGoal drives the agent's goal loop (assess → execute → evaluate cycles)
// over the session history. It mirrors Run's interrupt handling — permission
// and question interrupts park at the harness, answers resume the goal graph,
// which re-enters the interrupted execute cycle and its nested agent graph —
// and renders every step (cycle headers, execution deltas, tool use, verdict
// lines) through the same Renderer. The loop itself decides when to stop
// (verified completion, blocked, paused, or budget exhaustion); unlike Run
// there are no stop-hook or token-budget auto-continuations at this layer,
// because continuation IS the loop.
func (r *Runner) RunGoal(ctx context.Context, history []messages.Message) (final []messages.Message, err error) {
	// Goal-mode auto-retain: goal runs bypass Run (they execute the goal
	// graph directly), so this defer is their only hook into memory. It fires
	// on every return path of an invocation that actually drove the loop — a
	// refused start (goal already paused/cleared, loop already running) never
	// ran and retains nothing. Detached ctx so graph teardown doesn't cancel
	// the writes.
	started := false
	var runID uint64
	defer func() {
		if !started {
			return
		}
		// Release the run bookkeeping on every exit path: an abandoned loop
		// (error, cancellation, an interrupt nobody answers) must not leave
		// the agent reporting "already running" forever. No-op when the loop
		// ended cleanly or a newer run already took over.
		r.agent.AbandonGoalRun(runID)
		g := r.agent.Goal()
		if g == nil {
			return // cleared before the loop ended: nothing to retain
		}
		r.agent.RetainGoal(context.WithoutCancel(ctx), g.Objective, string(g.Status), goalSummary(g), err)
	}()
	stream, err := r.agent.GoalStream(ctx, history)
	if err != nil {
		return nil, err
	}
	started = true
	runID = r.agent.GoalRunID()
	defer stream.Close()
	for {
		step, ok, err := stream.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("runner: goal stream ended without a terminal step")
		}
		if step.Done {
			if r.render != nil {
				r.render(step)
			}
			return agent.MessagesFrom(step.Values), nil
		}
		if step.Err != nil {
			ie, isInterrupt := graph.AsInterrupt(step.Err)
			if !isInterrupt {
				return nil, step.Err
			}
			if r.handle == nil {
				return nil, step.Err
			}
			answer, err := r.handle(ie.Value)
			if err != nil {
				return nil, err
			}
			if r.RenderSummary != nil {
				for _, s := range r.agent.TakeReadyToolUseSummaries() {
					r.RenderSummary(s)
				}
			}
			stream.Close()
			stream, err = r.agent.GoalResume(ctx, ie, answer)
			if err != nil {
				return nil, err
			}
			defer stream.Close()
			continue
		}
		if r.render != nil {
			r.render(step)
		}
		if r.RenderSummary != nil {
			for _, s := range r.agent.TakeReadyToolUseSummaries() {
				r.RenderSummary(s)
			}
		}
	}
}
