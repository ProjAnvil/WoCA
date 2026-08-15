package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/graph"
)

// The goal graph: an explicit langgraph-style orchestration of the
// persistent-objective loop.
//
//	assess → execute → evaluate ─┬─ (active)   → assess (next cycle)
//	                              └─ (terminal) → END
//
// assess and evaluate are focused single model calls (no tools); execute is a
// streaming node that runs the REAL agent graph (model ⇄ tools, permissions,
// hooks, compaction) as a nested sub-execution on the live session history,
// forwarding its deltas so the harness renders goal cycles exactly like a
// normal run. evaluate is the verification gate: a skeptical evaluator must
// confirm any completion claim with evidence before the goal may complete.
// Interrupts raised inside execute (permissions, AskUserQuestion) are
// surfaced to the harness as fresh interrupts and resumed through the stashed
// inner checkpoint, so human-in-the-loop approvals work mid-cycle.

const (
	goalNodeAssess   = "assess"
	goalNodeExecute  = "execute"
	goalNodeEvaluate = "evaluate"
)

// GoalDelta forwards one message produced inside a goal execute cycle (tool
// calls, tool results) through the goal graph's delta channel so UI renderers
// can display goal-cycle tool use live, exactly like a normal run.
type GoalDelta struct {
	Message messages.Message
}

// GoalStream starts the goal loop over the session history and streams its
// progress. The harness pumps it like Agent.Stream, renders steps, and
// resolves interrupts via GoalResume.
func (a *Agent) GoalStream(ctx context.Context, history []messages.Message) (graph.Stream[graph.Step], error) {
	a.goal.mu.Lock()
	if a.goal.goal == nil {
		a.goal.mu.Unlock()
		return nil, fmt.Errorf("agent: no goal set")
	}
	if a.goal.goal.Status != GoalActive {
		a.goal.mu.Unlock()
		return nil, fmt.Errorf("agent: goal is %s (resume it first)", a.goal.goal.Status)
	}
	if a.goal.running {
		a.goal.mu.Unlock()
		return nil, fmt.Errorf("agent: goal loop is already running")
	}
	g := a.buildGoalGraph(a.goal.goal.MaxCycles)
	a.goal.graph = g
	a.goal.running = true
	a.goal.pending = nil
	a.goal.mu.Unlock()

	inner, err := g.Stream(ctx, a.NewState(history))
	if err != nil {
		a.endGoalRun()
		return nil, err
	}
	return &goalStream{a: a, inner: inner}, nil
}

// GoalResume continues a goal loop paused at an interrupt, feeding answer
// back into the interrupted execute cycle.
func (a *Agent) GoalResume(ctx context.Context, ie *graph.InterruptError, answer any) (graph.Stream[graph.Step], error) {
	a.goal.mu.Lock()
	g := a.goal.graph
	running := a.goal.running
	a.goal.mu.Unlock()
	if g == nil || !running {
		return nil, fmt.Errorf("agent: no goal run in flight")
	}
	inner, err := g.StreamResume(ctx, ie, answer)
	if err != nil {
		a.endGoalRun()
		return nil, err
	}
	return &goalStream{a: a, inner: inner}, nil
}

// goalStream wraps the compiled goal graph stream, clearing the goal-run
// bookkeeping when the stream reaches a terminal state (done, error, or an
// abandoned/cancelled pump).
type goalStream struct {
	a     *Agent
	inner graph.Stream[graph.Step]
	done  bool
}

func (s *goalStream) Next(ctx context.Context) (graph.Step, bool, error) {
	step, ok, err := s.inner.Next(ctx)
	if err != nil || !ok || step.Done {
		if !s.done {
			s.done = true
			s.a.endGoalRun()
		}
		return step, ok, err
	}
	if step.Err != nil {
		// A terminal interrupt parks the loop for an answer; the run stays
		// alive so GoalResume can continue it. Any other error ends it.
		if _, isInterrupt := graph.AsInterrupt(step.Err); !isInterrupt {
			if !s.done {
				s.done = true
				s.a.endGoalRun()
			}
		}
	}
	return step, ok, err
}

func (s *goalStream) Close() error { return s.inner.Close() }

func (a *Agent) endGoalRun() {
	a.goal.mu.Lock()
	a.goal.running = false
	a.goal.pending = nil
	a.goal.graph = nil
	a.goal.mu.Unlock()
}

// buildGoalGraph compiles the assess → execute → evaluate loop. The recursion
// limit bounds dispatches, not cycles; each cycle costs three dispatches.
func (a *Agent) buildGoalGraph(maxCycles int) *graph.CompiledGraph {
	g := graph.NewStateGraph()
	g.AddReducer("messages", MessagesReducer)
	g.AddStreamingNode(goalNodeAssess, a.goalAssessNode())
	g.AddStreamingNode(goalNodeExecute, a.goalExecuteNode())
	g.AddStreamingNode(goalNodeEvaluate, a.goalEvaluateNode())
	g.AddEdge(goalNodeAssess, goalNodeExecute)
	g.AddEdge(goalNodeExecute, goalNodeEvaluate)
	g.AddConditionalEdges(goalNodeEvaluate, a.goalRoute)
	g.SetEntryPoint(goalNodeAssess)
	limit := maxCycles*3 + 8
	opts := []graph.CompileOption{graph.WithRecursionLimit(limit)}
	if a.obs != nil {
		opts = append(opts, graph.WithObserver("goal", a.obs))
	}
	cg, err := g.Compile(opts...)
	if err != nil {
		// The graph is valid by construction; a compile failure is a bug.
		panic(fmt.Sprintf("agent: compile goal graph: %v", err))
	}
	return cg
}

// goalRoute ends the loop unless the goal is still active (the evaluate node
// applies every terminal transition itself).
func (a *Agent) goalRoute(_ context.Context, _ graph.State) (string, error) {
	a.goal.mu.Lock()
	g := a.goal.goal
	a.goal.mu.Unlock()
	if g == nil || g.Status != GoalActive {
		return graph.END, nil
	}
	return goalNodeAssess, nil
}

// goalCycleMsg builds the deterministic <goal-cycle> human message that seeds
// each execute cycle. It must be byte-identical across a resume re-entry.
func goalCycleMsg(cycle int, maxCycles int, objective, assessment string, tokensUsed, maxTokens int64) messages.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "<goal-cycle %d>\n", cycle)
	b.WriteString("You are autonomously pursuing this goal:\n\n" + objective + "\n\n")
	b.WriteString("Plan and status from the assessment stage:\n\n" + assessment + "\n\n")
	b.WriteString("Work on this cycle's actions now. Use tools to make real changes and verify them (run tests, read files). " +
		"Before finishing the cycle call UpdateGoal: action \"progress\" with what you did, action \"claim_complete\" with evidence if you believe the goal is fully achieved " +
		"(an independent evaluator will verify), or action \"blocked\" with the reason if you cannot proceed.\n")
	fmt.Fprintf(&b, "Budget: cycle %d of %d · %s of %s tokens used.\n", cycle, maxCycles, formatTokens(tokensUsed), formatTokens(maxTokens))
	b.WriteString("</goal-cycle>")
	return messages.Human(b.String())
}

// FormatGoalTokens renders a token count compactly for UIs (1200000 → "1M").
func FormatGoalTokens(n int64) string { return formatTokens(n) }

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// goalModel returns the model used for the assess/evaluate single calls. It
// is the same tool-bound runtime model the inner graph uses (sharing its
// retry/fallback router and, for scripted fakes, its response cursor), so the
// whole loop draws from one deterministic sequence.
func (a *Agent) goalModel() language.ChatModel {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.boundModel
}

// goalAssessNode is the planner: a focused model call that assesses progress
// against the objective and lays out this cycle's actions.
func (a *Agent) goalAssessNode() graph.StreamingNodeFunc {
	return func(ctx context.Context, state graph.State, emit func(any)) (any, error) {
		if !a.goalBeginCycle() {
			// Cleared or paused since the last node: end the loop.
			return graph.Command{Goto: graph.END}, nil
		}
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		a.goal.mu.Unlock()

		var b strings.Builder
		b.WriteString("You are the planning stage of a goal-driven agent loop.\n\n")
		fmt.Fprintf(&b, "Goal: %s\n", goal.Objective)
		fmt.Fprintf(&b, "Cycle: %d of at most %d · tokens used: %s of %s\n\n", goal.Cycles, goal.MaxCycles, formatTokens(goal.TokensUsed), formatTokens(goal.MaxTokens))
		if len(goal.Progress) == 0 {
			b.WriteString("Progress so far: none yet — this is the first cycle. Start by proposing 3-6 concrete, verifiable done-criteria for the goal as a markdown checklist, then plan the first actions.\n\n")
		} else {
			b.WriteString("Progress so far (most recent last):\n")
			start := len(goal.Progress) - 5
			if start < 0 {
				start = 0
			}
			for _, p := range goal.Progress[start:] {
				fmt.Fprintf(&b, "  cycle %d [%s] %s\n", p.Cycle, p.Verdict, p.Summary)
			}
			if len(goal.Notes) > 0 {
				b.WriteString("Executor notes:\n")
				for _, n := range goal.Notes[len(goal.Notes)-3:] {
					fmt.Fprintf(&b, "  - %s\n", goalTruncate(n, 200))
				}
			}
			b.WriteString("\nReuse and refine the original done-criteria checklist; mark verified items with [x].\n\n")
		}
		b.WriteString("Recent conversation tail (for context):\n" + goalMessageTail(MessagesFrom(state), 8, 160) + "\n\n")
		b.WriteString("Respond with:\n1. A short status assessment (2-4 sentences): what is verified done, what remains.\n2. The done-criteria checklist.\n3. A line \"This cycle:\" followed by 1-5 concrete actions to advance the goal.")

		resp, err := a.goalModel().Invoke(ctx, []messages.Message{messages.Human(b.String())})
		if err != nil {
			return nil, err
		}
		a.usage.recordUsage(resp.UsageMetadata)
		assessment := strings.TrimSpace(resp.Content)
		if assessment == "" {
			assessment = "(no assessment produced; proceeding to execution)"
		}
		if emit != nil {
			emit(fmt.Sprintf("\n[goal cycle %d/%d] %s\n", goal.Cycles, goal.MaxCycles, goalTruncate(firstLine(assessment), 200)))
		}
		return graph.State{"assessment": assessment}, nil
	}
}

// goalBeginCycle advances the cycle counter (notifying subscribers) and
// reports whether the loop should keep running.
func (a *Agent) goalBeginCycle() bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil || g.Status != GoalActive {
		a.goal.mu.Unlock()
		return false
	}
	g.Cycles++
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// goalExecuteNode runs one full agent turn (the nested model ⇄ tools graph)
// against the live session history, seeded with the cycle prompt. Inner
// interrupts are surfaced to the harness as fresh interrupt errors; the inner
// checkpoint is stashed so a resume re-enters the inner graph mid-cycle.
func (a *Agent) goalExecuteNode() graph.StreamingNodeFunc {
	return func(ctx context.Context, state graph.State, emit func(any)) (any, error) {
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		pending := a.goal.pending
		pendingTurns := a.goal.pendingTurns
		pendingPrefix := a.goal.pendingPrefix
		a.goal.mu.Unlock()
		if goal == nil || goal.Status != GoalActive {
			return graph.Command{Goto: graph.END}, nil
		}
		assessment, _ := state["assessment"].(string)

		msgs := MessagesFrom(state)
		hist := msgs
		if len(hist) > 0 && hist[0].Role == messages.RoleSystem {
			hist = hist[1:]
		}

		var stream graph.Stream[graph.Step]
		var prefix int
		var turnsBefore int
		if pending != nil {
			// Resume path: re-surface the stashed interrupt; on the resumed
			// pass graph.Interrupt returns the harness's answer, which feeds
			// the inner graph's StreamResume. The cycle prompt and prefix are
			// taken from the stash — they must not be rebuilt (compaction
			// could re-summarize differently).
			answer, err := graph.Interrupt(ctx, pending.Value)
			if err != nil {
				return nil, err
			}
			s, err := a.graph.StreamResume(ctx, pending, answer)
			if err != nil {
				return nil, err
			}
			stream = s
			prefix = pendingPrefix
			turnsBefore = pendingTurns
			a.goal.setPending(nil, 0, 0)
		} else {
			hist = a.CompactHistory(ctx, hist)
			cycleMsg := goalCycleMsg(goal.Cycles, goal.MaxCycles, goal.Objective, assessment, goal.TokensUsed, goal.MaxTokens)
			prefix = len(hist)
			s, err := a.graph.Stream(ctx, a.NewState(append(hist, cycleMsg)))
			if err != nil {
				return nil, err
			}
			stream = s
			a.ResetTurnCounter()
			turnsBefore = 0
		}

		var newMsgs []messages.Message
		pump := func() error {
			for {
				step, ok, err := stream.Next(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("agent: goal execute stream ended without a terminal step")
				}
				if step.Done {
					final := MessagesFrom(step.Values)
					// final = [system] + prefix history + this cycle's messages.
					if len(final) > prefix+1 {
						newMsgs = final[prefix+1:]
					}
					return nil
				}
				if step.Err != nil {
					if ie, isInterrupt := graph.AsInterrupt(step.Err); isInterrupt {
						a.goal.setPending(ie, turnsBefore, prefix)
						// A fresh interrupt carries the same payload; the
						// outer runtime will attach ITS checkpoint to it, so
						// the inner one stays resumable.
						return &graph.InterruptError{Value: ie.Value}
					}
					return step.Err
				}
				if emit == nil {
					continue
				}
				if s, ok := step.Delta.(string); ok && s != "" {
					emit(s)
					continue
				}
				for _, m := range MessagesFrom(step.Update) {
					if m.Role == messages.RoleAI && len(m.ToolCalls) > 0 || m.Role == messages.RoleTool {
						emit(GoalDelta{Message: m})
					}
				}
			}
		}
		if err := pump(); err != nil {
			return nil, err
		}
		cycleTurns := a.TurnsDone() - turnsBefore
		return graph.State{
			"messages":    newMsgs,
			"cycle_turns": cycleTurns,
		}, nil
	}
}

// goalEvaluateNode is the verification gate: a skeptical evaluator reviews the
// cycle's evidence and moves the goal state machine. Only a verified verdict
// completes the goal; claims alone never do.
func (a *Agent) goalEvaluateNode() graph.StreamingNodeFunc {
	return func(ctx context.Context, state graph.State, emit func(any)) (any, error) {
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		a.goal.mu.Unlock()
		if goal == nil {
			return graph.Command{Goto: graph.END}, nil
		}
		assessment, _ := state["assessment"].(string)
		cycleTurns, _ := state["cycle_turns"].(int)

		a.goalRefreshTokens(goal.usageBase)

		var b strings.Builder
		b.WriteString("You are a strict, skeptical evaluator verifying whether a coding goal has been achieved. Trust only evidence (tool results, test output, file contents) — never the executor's claims.\n\n")
		fmt.Fprintf(&b, "Goal: %s\n", goal.Objective)
		fmt.Fprintf(&b, "Cycle %d of %d · %s of %s tokens used.\n", goal.Cycles, goal.MaxCycles, formatTokens(goal.TokensUsed), formatTokens(goal.MaxTokens))
		if goal.Claimed {
			fmt.Fprintf(&b, "\nThe executor CLAIMS completion: %s\nVerify this claim against the evidence ruthlessly.\n", goalTruncate(goal.ClaimEvidence, 500))
		}
		fmt.Fprintf(&b, "\nThis cycle's plan and status:\n%s\n", goalTruncate(assessment, 2000))
		fmt.Fprintf(&b, "\nEvidence from this cycle's work (tool results and executor output):\n%s\n", goalMessageTail(goalCycleMessages(MessagesFrom(state)), 30, 300))
		b.WriteString(`
Respond with ONLY a JSON object:
{"verdict":"complete|continue|blocked","confidence":0.0,"evidence":"what was concretely verified","gaps":"what is missing (for continue)","next_step":"the single most valuable next action"}

Rules: "complete" ONLY if the evidence concretely shows every done-criterion met (tests run and passing, files verified, commands succeeded). If the executor claimed completion but the evidence does not fully support it, reply "continue" and list what is missing. When in doubt, "continue". "blocked" only for hard blockers needing human input.`)

		resp, err := a.goalModel().Invoke(ctx, []messages.Message{messages.Human(b.String())})
		if err != nil {
			return nil, err
		}
		a.usage.recordUsage(resp.UsageMetadata)
		v := parseGoalVerdict(resp.Content)
		if emit != nil {
			emit(fmt.Sprintf("\n[goal verdict: %s] %s\n", v.Verdict, goalTruncate(orString(v.Evidence, v.Gaps), 200)))
		}

		a.goal.mu.Lock()
		g := a.goal.goal
		if g == nil {
			a.goal.mu.Unlock()
			return graph.Command{Goto: graph.END}, nil
		}
		g.Progress = append(g.Progress, GoalProgressEntry{
			Cycle:   g.Cycles,
			Summary: goalTruncate(firstLine(assessment), 160),
			Verdict: v.Verdict,
			Note:    goalTruncate(orString(v.Evidence, v.Gaps), 240),
		})
		if len(g.Progress) > 50 {
			g.Progress = g.Progress[len(g.Progress)-50:]
		}
		a.goal.mu.Unlock()

		// A mid-cycle blocked (UpdateGoal action=blocked) or a user pause
		// wins over the verdict.
		a.goal.mu.Lock()
		status := a.goal.goal.Status
		a.goal.mu.Unlock()
		switch {
		case status != GoalActive:
			// already paused/blocked/cleared: report and stop
		case v.Verdict == "complete":
			a.goalSetStatus(GoalComplete, "", orString(v.Evidence, "verified by evaluator"))
		case v.Verdict == "blocked":
			a.goalSetStatus(GoalBlocked, orString(v.Gaps, v.Evidence), "")
		default:
			// continue: claim not verified, or ordinary progress.
			a.goalResetClaim()
			if goal.MaxCycles > 0 && goal.Cycles >= goal.MaxCycles {
				a.goalSetStatus(GoalBudgetLimited, fmt.Sprintf("cycle budget reached (%d cycles)", goal.MaxCycles), "")
			} else if goal.MaxTokens > 0 && goal.TokensUsed >= goal.MaxTokens {
				a.goalSetStatus(GoalBudgetLimited, fmt.Sprintf("token budget reached (%s tokens)", formatTokens(goal.MaxTokens)), "")
			} else if cycleTurns <= 0 {
				// Empty-cycle suppression (Codex's no-tool-call guard): a
				// cycle that ran no tool rounds makes no verifiable progress;
				// after two in a row the goal is parked as blocked instead of
				// burning budget on chatter.
				if a.goalStall() {
					a.goalSetStatus(GoalBlocked, "no observable progress for two consecutive cycles (no tool use)", "")
				}
			} else {
				a.goalClearStall()
			}
		}

		// Terminal cycles close with a visible report in the transcript.
		a.goal.mu.Lock()
		finalStatus := a.goal.goal.Status
		a.goal.mu.Unlock()
		if finalStatus != GoalActive {
			report := goalReport(finalStatus, v, goal.Cycles)
			return graph.State{"messages": []messages.Message{messages.AI(report)}, "verdict": v.Verdict}, nil
		}
		return graph.State{"verdict": v.Verdict}, nil
	}
}

// goalResetClaim clears an unverified completion claim so the executor must
// re-claim (and re-provide evidence) next cycle.
func (a *Agent) goalResetClaim() {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g != nil && g.Claimed {
		g.Claimed = false
		g.UpdatedAt = time.Now()
	}
	a.goal.mu.Unlock()
}

// goalStall counts consecutive no-tool cycles; it reports true at the limit.
func (a *Agent) goalStall() bool {
	a.goal.mu.Lock()
	defer a.goal.mu.Unlock()
	if a.goal.goal == nil {
		return false
	}
	a.goal.goal.stalls++
	return a.goal.goal.stalls >= 2
}

func (a *Agent) goalClearStall() {
	a.goal.mu.Lock()
	if a.goal.goal != nil {
		a.goal.goal.stalls = 0
	}
	a.goal.mu.Unlock()
}

// goalRefreshTokens recomputes TokensUsed from the session usage delta.
func (a *Agent) goalRefreshTokens(base int64) {
	total := int64(a.usage.snapshot().TotalTokens)
	a.goal.mu.Lock()
	if a.goal.goal != nil {
		used := total - base
		if used < 0 {
			used = 0
		}
		a.goal.goal.TokensUsed = used
	}
	a.goal.mu.Unlock()
}

func (m *goalManager) setPending(ie *graph.InterruptError, turns, prefix int) {
	m.mu.Lock()
	m.pending = ie
	m.pendingTurns = turns
	m.pendingPrefix = prefix
	m.mu.Unlock()
}

// goalVerdict is the evaluator's structured output.
type goalVerdict struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
	Gaps       string  `json:"gaps"`
	NextStep   string  `json:"next_step"`
}

// parseGoalVerdict leniently extracts the evaluator's JSON object (models
// occasionally wrap it in prose or code fences). Unparsable output defaults
// to "continue" — the verification gate fails closed.
func parseGoalVerdict(s string) goalVerdict {
	v := goalVerdict{Verdict: "continue"}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		v.Evidence = goalTruncate(s, 300)
		return v
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		v = goalVerdict{Verdict: "continue", Evidence: goalTruncate(s, 300)}
		return v
	}
	switch v.Verdict {
	case "complete", "continue", "blocked":
	default:
		v.Verdict = "continue"
	}
	return v
}

func goalReport(status GoalStatus, v goalVerdict, cycle int) string {
	switch status {
	case GoalComplete:
		return fmt.Sprintf("🎯 Goal complete (cycle %d).\nEvidence: %s", cycle, orString(v.Evidence, "verified by evaluator"))
	case GoalBlocked:
		return fmt.Sprintf("⛔ Goal blocked (cycle %d): %s", cycle, orString(v.Gaps, v.Evidence))
	case GoalPaused:
		return fmt.Sprintf("⏸ Goal paused by user (cycle %d). Resume with /goal resume.", cycle)
	case GoalBudgetLimited:
		return fmt.Sprintf("⏸ Goal budget limited (cycle %d). Adjust with /goal budget and resume.", cycle)
	}
	return fmt.Sprintf("Goal loop ended (cycle %d, status %s).", cycle, status)
}

// goalCycleMessages returns this cycle's messages: everything from the last
// <goal-cycle> human message onward.
func goalCycleMessages(msgs []messages.Message) []messages.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleHuman && strings.HasPrefix(msgs[i].Content, "<goal-cycle") {
			return msgs[i:]
		}
	}
	if len(msgs) > 30 {
		return msgs[len(msgs)-30:]
	}
	return msgs
}

// goalMessageTail renders a bounded preview of the last max messages.
func goalMessageTail(msgs []messages.Message, max, perMsg int) string {
	if len(msgs) == 0 {
		return "(empty)"
	}
	if len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleHuman:
			fmt.Fprintf(&b, "user: %s\n", goalTruncate(m.Content, perMsg))
		case messages.RoleAI:
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "assistant called %s(%s)\n", tc.Name, goalTruncate(fmt.Sprintf("%v", tc.Args), 120))
				}
			}
			if m.Content != "" {
				fmt.Fprintf(&b, "assistant: %s\n", goalTruncate(m.Content, perMsg))
			}
		case messages.RoleTool:
			fmt.Fprintf(&b, "tool result: %s\n", goalTruncate(m.Content, perMsg))
		}
	}
	return b.String()
}

func goalTruncate(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func orString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
