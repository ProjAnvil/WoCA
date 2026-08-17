// Package runner drives an agent run: it streams graph progress chunks,
// renders them, and resolves permission interrupts through a caller-supplied
// handler.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agent"
)

// InterruptHandler resolves a graph interrupt into a resume value. The value
// is one of agent.PermissionRequest, agent.AskUserRequest, or
// agent.PlanApprovalRequest (decoded from its envelope).
type InterruptHandler func(value any) (any, error)

// Renderer observes one stream chunk (used for live progress display).
type Renderer func(chunk lggraph.StreamChunk)

// Runner executes an agent while handling interrupts and rendering progress.
type Runner struct {
	agent  *agent.Agent
	handle InterruptHandler
	render Renderer
	// RenderSummary, when set, receives async tool-use summaries as they
	// settle (consumed non-blockingly while rounds stream, plus a bounded
	// wait at turn end).
	RenderSummary func(agent.ToolUseSummary)
	// TokenBudget is the turn's cumulative output-token budget for
	// auto-continue; 0 disables. Opt in per
	// environment — normal conversations should end naturally.
	TokenBudget int
}

// New creates a runner.
func New(a *agent.Agent, handle InterruptHandler, render Renderer) *Runner {
	return &Runner{agent: a, handle: handle, render: render}
}

// chunkInterrupts extracts the pause interrupts from an updates- or
// values-chunk payload, or nil when the chunk is not a pause.
func chunkInterrupts(chunk lggraph.StreamChunk) []lgtypes.Interrupt {
	var v any
	switch chunk.Mode {
	case lggraph.StreamUpdates, lggraph.StreamValues:
		v = chunk.Payload
	default:
		return nil
	}
	m, _ := v.(map[string]any)
	intrs, _ := m[lgcheckpoint.ReservedInterrupt].([]lgtypes.Interrupt)
	return intrs
}

// updateMessages flattens an updates-chunk payload ({node: {channel: value}})
// into its message batch (the graphs are single-task per superstep).
func updateMessages(payload any) []messages.Message {
	updates, _ := payload.(map[string]any)
	for _, nodeUpdate := range updates {
		state, _ := nodeUpdate.(map[string]any)
		if msgs := agent.MessagesFrom(state); len(msgs) > 0 {
			return msgs
		}
	}
	return nil
}

// Run executes the agent over history (which must NOT include the system
// prompt) and returns the full transcript including the leading system
// message. Permission interrupts are resolved through the handler; the run
// resumes from its checkpoint thread after each answer. On natural completion,
// Stop hooks run and may inject feedback to continue the turn.
func (r *Runner) Run(ctx context.Context, history []messages.Message) (final []messages.Message, err error) {
	// One run = one memory-prefetch scope: concurrent runs on the shared
	// agent each get their own slot (see agent.WithMemoryScope).
	ctx = agent.WithMemoryScope(ctx)
	// Auto-retain (single write choke point): every return path stores the
	// run's Q/A + outcome into long-term memory. Detached ctx so graph
	// teardown doesn't cancel the writes.
	defer func() {
		r.agent.RetainRun(context.WithoutCancel(ctx), history, final, err)
	}()
	hist := history
	// One Run = one turn for the agent's MaxTurns accounting.
	r.agent.ResetTurnCounter()
	thread := agent.NewThreadID()
	stream := r.agent.Stream(ctx, hist, thread)
	stopContinuations := 0
	outputRecoveries := 0
	ptlAttempted := false
	var outputRestore func()
	defer func() {
		if outputRestore != nil {
			outputRestore()
		}
	}()
	budgetTracker := tokenBudgetTracker{}
	seenHist := len(hist)
	turnTokens := 0
	for {
		paused, values, streamErr := r.pump(ctx, stream)
		if streamErr != nil {
			// Prompt-too-long recovery: compact once and retry the turn
			// on the compacted history (single-attempt guard).
			if !ptlAttempted && agent.IsPromptTooLongError(streamErr) {
				ptlAttempted = true
				compacted := r.agent.ReactiveCompactHistory(ctx, hist)
				if agent.CompactionShrank(hist, compacted) {
					hist = compacted
					stream = r.agent.Stream(ctx, hist, thread)
					continue
				}
			}
			return nil, streamErr
		}
		if len(paused) > 0 {
			if r.handle == nil {
				return nil, &lgtypes.GraphInterrupt{Interrupt: paused[0]}
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
			stream = r.agent.ResumeStream(ctx, thread, answer)
			continue
		}
		// Completed: values holds the final state.
		// Final batch's summary: bounded wait so it is not lost at turn
		// end (await the pending summary after the stream).
		if r.RenderSummary != nil {
			wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
			for _, s := range r.agent.WaitToolUseSummaries(wctx) {
				r.RenderSummary(s)
			}
			wcancel()
		}
		msgs := agent.MessagesFrom(values)

		// Max-output-tokens recovery, ahead of Stop hooks (stop hooks
		// are skipped on a truncated tail): retry once at the
		// escalated cap, then with continuation messages.
		if agent.TruncatedTail(msgs) {
			if outputRestore == nil {
				if restore, ok := r.agent.BeginOutputEscalation(); ok {
					outputRestore = restore
					stream = r.agent.Stream(ctx, hist, thread)
					continue
				}
			}
			if outputRecoveries < agent.MaxOutputTokensRecoveries {
				outputRecoveries++
				hist = historyFrom(msgs)
				hist = append(hist, messages.Human(agent.OutputLimitRecoveryPrompt))
				stream = r.agent.Stream(ctx, hist, thread)
				continue
			}
			return msgs, nil
		}

		feedback, blocked, err := r.agent.RunStopHooks(ctx, "completed")
		if err != nil {
			return nil, err
		}
		if blocked && feedback != "" && stopContinuations < 3 {
			stopContinuations++
			hist = historyFrom(msgs)
			hist = append(hist, messages.Human("<stop-hook>\n"+feedback+"\n</stop-hook>"))
			stream = r.agent.Stream(ctx, hist, thread)
			continue
		}
		// Token-budget auto-continue (after the
		// stop hooks): with a budget configured and under 90% spent, nudge
		// the model to keep working until the budget or diminishing
		// returns (3+ continuations with <500-token deltas) stop it.
		if r.TokenBudget > 0 {
			turnTokens += estimateTurnOutputTokens(historyFrom(msgs)[seenHist:])
			decision := checkTokenBudget(&budgetTracker, r.TokenBudget, turnTokens)
			if decision.Continue {
				hist = historyFrom(msgs)
				hist = append(hist, messages.Human(decision.Nudge))
				seenHist = len(hist)
				stream = r.agent.Stream(ctx, hist, thread)
				continue
			}
		}
		return msgs, nil
	}
}

// pump consumes one stream to its end, rendering as it goes. It reports the
// pause interrupts (non-empty when the run paused), the final values snapshot
// (nil on pause or error), and the terminal stream error.
func (r *Runner) pump(ctx context.Context, stream iter.Seq2[lggraph.StreamChunk, error]) (paused []lgtypes.Interrupt, values map[string]any, streamErr error) {
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

// valuesState extracts the channel-state map from a values-chunk payload.
func valuesState(payload any) map[string]any {
	state, _ := payload.(map[string]any)
	return state
}

// historyFrom strips a leading system message from a transcript.
func historyFrom(msgs []messages.Message) []messages.Message {
	if len(msgs) > 0 && msgs[0].Role == messages.RoleSystem {
		return msgs[1:]
	}
	return msgs
}

// DefaultRenderer renders stream chunks as human-readable terminal output:
// streamed text deltas, tool-call summaries, and tool-result previews.
func DefaultRenderer(w io.Writer) Renderer {
	streaming := false
	return func(chunk lggraph.StreamChunk) {
		switch chunk.Mode {
		case lggraph.StreamCustom:
			if s, ok := chunk.Payload.(string); ok && s != "" {
				fmt.Fprint(w, s)
				streaming = true
				return
			}
			if rd, ok := chunk.Payload.(agent.ReasoningDelta); ok && rd.Text != "" {
				return // reasoning hidden in the default renderer
			}
		case lggraph.StreamUpdates:
			if streaming {
				fmt.Fprintln(w)
				streaming = false
			}
			for _, m := range updateMessages(chunk.Payload) {
				switch m.Role {
				case messages.RoleAI:
					for _, tc := range m.ToolCalls {
						fmt.Fprintf(w, "\n⏺ %s(%s)\n", tc.Name, summarizeArgs(tc.Args))
					}
				case messages.RoleTool:
					fmt.Fprintf(w, "  ↳ %s\n", summarizeResult(m.Content))
				}
			}
		}
	}
}

// DefaultSummaryRenderer renders async tool-use summary labels as one line
// per completed batch.
func DefaultSummaryRenderer(w io.Writer) func(agent.ToolUseSummary) {
	return func(s agent.ToolUseSummary) {
		if s.Text != "" {
			fmt.Fprintf(w, "  · %s\n", s.Text)
		}
	}
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "..."
	}
	s := string(data)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func summarizeResult(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "(no output)"
	}
	first := content
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		first = content[:idx]
	}
	if len(first) > 200 {
		first = first[:200] + "…"
	}
	return first
}
