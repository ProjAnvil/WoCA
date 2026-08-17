// Package runner drives an agent run: it streams graph progress, renders it,
// and resolves permission interrupts through a caller-supplied handler.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/graph"
)

// InterruptHandler resolves a graph interrupt into a resume value. The value
// is one of agent.PermissionRequest, agent.AskUserRequest, or
// agent.PlanApprovalRequest.
type InterruptHandler func(value any) (any, error)

// Renderer observes one graph step (used for live progress display).
type Renderer func(step graph.Step)

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
	// auto-continue (Claude Code's 500k feature); 0 disables. Opt in per
	// environment — normal conversations should end naturally.
	TokenBudget int
}

// New creates a runner.
func New(a *agent.Agent, handle InterruptHandler, render Renderer) *Runner {
	return &Runner{agent: a, handle: handle, render: render}
}

// Run executes the agent over history (which must NOT include the system
// prompt) and returns the full transcript including the leading system
// message. Permission interrupts are resolved through the handler; the run is
// resumed from the graph checkpoint after each answer. On natural completion,
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
	stream, err := r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
	if err != nil {
		return nil, err
	}
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
		step, ok, err := stream.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("runner: stream ended without a terminal step")
		}
		if step.Done {
			if r.render != nil {
				r.render(step)
			}
			// Final batch's summary: bounded wait so it is not lost at turn
			// end (Claude Code awaits the pending summary after the stream).
			if r.RenderSummary != nil {
				wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
				for _, s := range r.agent.WaitToolUseSummaries(wctx) {
					r.RenderSummary(s)
				}
				wcancel()
			}
			msgs := agent.MessagesFrom(step.Values)

			// Max-output-tokens recovery, ahead of Stop hooks (Claude Code
			// skips stop hooks on a truncated tail): retry once at the
			// escalated cap, then with continuation messages.
			if agent.TruncatedTail(msgs) {
				if outputRestore == nil {
					if restore, ok := r.agent.BeginOutputEscalation(); ok {
						outputRestore = restore
						stream.Close()
						stream, err = r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
						if err != nil {
							return nil, err
						}
						continue
					}
				}
				if outputRecoveries < agent.MaxOutputTokensRecoveries {
					outputRecoveries++
					hist = historyFrom(msgs)
					hist = append(hist, messages.Human(agent.OutputLimitRecoveryPrompt))
					stream.Close()
					stream, err = r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
					if err != nil {
						return nil, err
					}
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
				full := agent.MessagesFrom(step.Values)
				hist = historyFrom(full)
				hist = append(hist, messages.Human("<stop-hook>\n"+feedback+"\n</stop-hook>"))
				stream.Close()
				stream, err = r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
				if err != nil {
					return nil, err
				}
				continue
			}
			// Token-budget auto-continue (Claude Code's stage 22e, after the
			// stop hooks): with a budget configured and under 90% spent, nudge
			// the model to keep working until the budget or diminishing
			// returns (3+ continuations with <500-token deltas) stop it.
			if r.TokenBudget > 0 {
				full := agent.MessagesFrom(step.Values)
				turnTokens += estimateTurnOutputTokens(historyFrom(full)[seenHist:])
				decision := checkTokenBudget(&budgetTracker, r.TokenBudget, turnTokens)
				if decision.Continue {
					hist = historyFrom(full)
					hist = append(hist, messages.Human(decision.Nudge))
					seenHist = len(hist)
					stream.Close()
					stream, err = r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
					if err != nil {
						return nil, err
					}
					continue
				}
			}
			return agent.MessagesFrom(step.Values), nil
		}
		if step.Err != nil {
			ie, isInterrupt := graph.AsInterrupt(step.Err)
			if !isInterrupt {
				// Prompt-too-long recovery: compact once and retry the turn
				// on the compacted history (single-attempt guard).
				if !ptlAttempted && agent.IsPromptTooLongError(step.Err) {
					ptlAttempted = true
					compacted := r.agent.ReactiveCompactHistory(ctx, hist)
					if agent.CompactionShrank(hist, compacted) {
						hist = compacted
						stream.Close()
						stream, err = r.agent.Graph().Stream(ctx, r.agent.NewState(ctx, hist))
						if err != nil {
							return nil, err
						}
						continue
					}
				}
				return nil, step.Err
			}
			if r.handle == nil {
				return nil, step.Err
			}
			answer, err := r.handle(ie.Value)
			if err != nil {
				return nil, err
			}
			stream.Close()
			stream, err = r.agent.Graph().StreamResume(ctx, ie, answer)
			if err != nil {
				return nil, err
			}
			continue
		}
		if r.render != nil {
			r.render(step)
		}
		// Tool-use summaries settle asynchronously: consume whatever is
		// ready while the next round streams (non-blocking).
		if r.RenderSummary != nil {
			for _, s := range r.agent.TakeReadyToolUseSummaries() {
				r.RenderSummary(s)
			}
		}
	}
}

// historyFrom strips a leading system message from a transcript.
func historyFrom(msgs []messages.Message) []messages.Message {
	if len(msgs) > 0 && msgs[0].Role == messages.RoleSystem {
		return msgs[1:]
	}
	return msgs
}

// DefaultRenderer renders graph steps as human-readable terminal output:
// streamed text deltas, tool-call summaries, and tool-result previews.
func DefaultRenderer(w io.Writer) Renderer {
	streaming := false
	return func(step graph.Step) {
		if s, ok := step.Delta.(string); ok && s != "" {
			fmt.Fprint(w, s)
			streaming = true
			return
		}
		if streaming {
			fmt.Fprintln(w)
			streaming = false
		}
		for _, m := range agent.MessagesFrom(step.Update) {
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
