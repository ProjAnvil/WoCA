package runner

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agent"
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
// and renders every chunk (cycle headers, execution deltas, tool use, verdict
// lines) through the same Renderer. The loop itself decides when to stop
// (verified completion, blocked, paused, or budget exhaustion); unlike Run
// there are no stop-hook or token-budget auto-continuations at this layer,
// because continuation IS the loop.
func (r *Runner) RunGoal(ctx context.Context, history []messages.Message) (final []messages.Message, err error) {
	// One goal run = one memory-prefetch scope (see agent.WithMemoryScope).
	ctx = agent.WithMemoryScope(ctx)
	// Goal-mode auto-retain: goal runs bypass Run (they execute the goal
	// graph directly), so this defer is their only hook into memory. It fires
	// on every return path of an invocation that actually drove the loop — a
	// refused start (goal already paused/cleared, loop already running) never
	// ran and retains nothing. Detached ctx so graph teardown doesn't cancel
	// the writes.
	started := false
	var runID uint64
	// File-mutation watermark: resolved into the goal run's changed-file
	// delta at retain time (the recall_ops operation record's resources).
	fhBase := r.agent.FileHistoryWatermark(ctx)
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
		r.agent.RetainGoal(context.WithoutCancel(ctx), g.Objective, string(g.Status), goalSummary(g), err,
			r.agent.FileChangesSince(ctx, fhBase))
	}()
	stream, err := r.agent.GoalStream(ctx, history)
	if err != nil {
		return nil, err
	}
	started = true
	runID = r.agent.GoalRunID()
	for {
		paused, values, streamErr := r.pumpGoal(ctx, stream)
		if streamErr != nil {
			return nil, streamErr
		}
		if len(paused) > 0 {
			if r.handle == nil {
				return nil, fmt.Errorf("runner: goal paused with no interrupt handler")
			}
			answer, err := r.handle(agent.DecodeInterruptValue(paused[0].Value))
			if err != nil {
				return nil, err
			}
			if r.RenderSummary != nil {
				for _, s := range r.agent.TakeReadyToolUseSummaries() {
					r.RenderSummary(s)
				}
			}
			stream, err = r.agent.GoalResume(ctx, answer)
			if err != nil {
				return nil, err
			}
			continue
		}
		// Completed: values holds the final goal-graph state.
		return agent.MessagesFrom(values), nil
	}
}

// pumpGoal consumes one goal stream to its end, rendering as it goes. It
// reports the pause interrupts (non-empty when the loop paused), the final
// values snapshot, and the terminal stream error.
func (r *Runner) pumpGoal(ctx context.Context, stream iter.Seq2[lggraph.StreamChunk, error]) (paused []lgtypes.Interrupt, values map[string]any, streamErr error) {
	for chunk, cerr := range stream {
		if cerr != nil {
			streamErr = cerr
			return
		}
		if intrs := chunkInterrupts(chunk); len(intrs) > 0 {
			paused = intrs
			return
		}
		switch chunk.Mode {
		case lggraph.StreamCustom, lggraph.StreamUpdates:
			if r.render != nil {
				r.render(chunk)
			}
			if r.RenderSummary != nil {
				for _, s := range r.agent.TakeReadyToolUseSummaries() {
					r.RenderSummary(s)
				}
			}
		case lggraph.StreamValues:
			values = valuesState(chunk.Payload)
		}
	}
	return
}
