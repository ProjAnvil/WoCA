package runner

import (
	"context"
	"fmt"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/graph"
)

// RunGoal drives the agent's goal loop (assess → execute → evaluate cycles)
// over the session history. It mirrors Run's interrupt handling — permission
// and question interrupts park at the harness, answers resume the goal graph,
// which re-enters the interrupted execute cycle and its nested agent graph —
// and renders every step (cycle headers, execution deltas, tool use, verdict
// lines) through the same Renderer. The loop itself decides when to stop
// (verified completion, blocked, paused, or budget exhaustion); unlike Run
// there are no stop-hook or token-budget auto-continuations at this layer,
// because continuation IS the loop.
func (r *Runner) RunGoal(ctx context.Context, history []messages.Message) ([]messages.Message, error) {
	stream, err := r.agent.GoalStream(ctx, history)
	if err != nil {
		return nil, err
	}
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
