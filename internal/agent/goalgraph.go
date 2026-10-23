package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/schema"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
)

// The goal graph: an explicit langgraph orchestration of the
// persistent-objective loop.
//
//	assess → execute → evaluate ─┬─ (active)   → assess (next cycle)
//	                              └─ (terminal) → END
//
// assess and evaluate are focused single model calls (no tools); execute is a
// node that runs the REAL agent graph (model ⇄ tools, permissions, hooks,
// compaction) as a nested execution on the live session history, forwarding
// its deltas so the harness renders goal cycles exactly like a normal run.
// evaluate is the verification gate: a skeptical evaluator must confirm any
// completion claim with evidence before the goal may complete. Interrupts
// raised inside execute (permissions, AskUserQuestion) pause the outer goal
// graph via lggraph.Interrupt carrying the same payload; the answer feeds the
// inner graph's resume (Options.Resume on the stashed inner thread), so
// human-in-the-loop approvals work mid-cycle.

const (
	goalNodeAssess   = "assess"
	goalNodeExecute  = "execute"
	goalNodeEvaluate = "evaluate"
)

// GoalDelta forwards one message produced inside a goal execute cycle (tool
// calls, tool results) through the goal graph's custom stream so UI renderers
// can display goal-cycle tool use live, exactly like a normal run.
type GoalDelta struct {
	Message messages.Message
}

// GoalNote is one fragment of goal-loop narration: cycle banners, verdict
// lines, cut notices, and the executor's streamed answer text inside a cycle.
// It is deliberately NOT a chat delta — intermediate cycle chatter must not
// read as assistant replies. Renderers surface it as loop progress (a dim
// status line, a collapsed card), never as the run's answer; the unified
// GoalFinalReply is the only goal text that reaches the chat.
type GoalNote struct {
	Text string
}

// GoalFinalReply is the single unified chat reply of a goal run: the
// executor's last substantive answer of the final cycle (for research-style
// goals that text is the actual deliverable) plus the terminal status report.
// Emitted exactly once, when the loop reaches a terminal state — the
// buffer-then-verify-then-answer shape Codex goal mode and Claude Code /goal
// use for their autonomous loops.
type GoalFinalReply struct {
	Text string
}

// goalInterrupts extracts the pause interrupts from an updates- or
// values-chunk payload, or nil when the chunk is not a pause.
func goalInterrupts(mode lggraph.StreamMode, payload any) []lgtypes.Interrupt {
	switch mode {
	case lggraph.StreamUpdates:
		updates, _ := payload.(map[string]any)
		intrs, _ := updates[lgcheckpoint.ReservedInterrupt].([]lgtypes.Interrupt)
		return intrs
	case lggraph.StreamValues:
		state, _ := payload.(map[string]any)
		intrs, _ := state[lgcheckpoint.ReservedInterrupt].([]lgtypes.Interrupt)
		return intrs
	}
	return nil
}

// GoalStream starts the goal loop over the session history and streams its
// progress. The harness pumps the returned chunk iterator like Agent.Stream,
// renders chunks, and resolves pauses via GoalResume.
func (a *Agent) GoalStream(ctx context.Context, history []messages.Message) (iter.Seq2[lggraph.StreamChunk, error], error) {
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
	a.goal.runID++
	run := a.goal.runID
	a.goal.graph = g
	a.goal.thread = fmt.Sprintf("goal-%d-%d", run, time.Now().UnixNano())
	a.goal.running = true
	a.goal.pending = nil
	a.goal.mu.Unlock()

	return a.goalChunkStream(ctx, history, g, run), nil
}

// GoalResume continues a goal loop paused at an interrupt, feeding answer
// back into the paused outer goal graph (which re-enters the execute node,
// where the answer reaches the inner agent graph's resume). The resumed
// stream keeps the same run id (it is the same run).
func (a *Agent) GoalResume(ctx context.Context, answer any) (iter.Seq2[lggraph.StreamChunk, error], error) {
	a.goal.mu.Lock()
	g := a.goal.graph
	running := a.goal.running
	thread := a.goal.thread
	a.goal.mu.Unlock()
	if g == nil || !running {
		return nil, fmt.Errorf("agent: no goal run in flight")
	}
	return func(yield func(lggraph.StreamChunk, error) bool) {
		paused := false
		defer func() {
			if !paused {
				a.endGoalRun()
			}
		}()
		for c, err := range g.Stream(ctx, nil, lggraph.StreamOptions{
			Options: lggraph.Options{ThreadID: thread, Resume: encodeResumeValue(answer)},
			Modes:   streamModes,
		}) {
			if err == nil && len(goalInterrupts(c.Mode, c.Payload)) > 0 {
				paused = true
			}
			if !yield(c, err) {
				return
			}
		}
	}, nil
}

// GoalRunID returns the id of the goal run in flight (0 when none). The
// runner captures it right after GoalStream and passes it to
// AbandonGoalRun when it exits.
func (a *Agent) GoalRunID() uint64 {
	a.goal.mu.Lock()
	defer a.goal.mu.Unlock()
	if !a.goal.running {
		return 0
	}
	return a.goal.runID
}

// AbandonGoalRun releases the goal-run bookkeeping if run is still the
// current goal run. The runner calls it on every exit path so an abandoned
// loop (error, cancellation, an interrupt nobody answers) cannot leave the
// agent reporting "already running" forever. Stale calls (a newer goal run
// already took over) are ignored.
func (a *Agent) AbandonGoalRun(run uint64) {
	if run == 0 {
		return
	}
	a.goal.mu.Lock()
	defer a.goal.mu.Unlock()
	if a.goal.runID != run {
		return
	}
	a.goal.running = false
	a.goal.pending = nil
	a.goal.graph = nil
}

// goalChunkStream wraps the goal graph's fresh-input stream, clearing the
// goal-run bookkeeping when the stream reaches a terminal state. A pause
// (interrupt chunk) keeps the run alive so GoalResume can continue it; an
// abandoned pump (consumer break) releases it.
func (a *Agent) goalChunkStream(ctx context.Context, history []messages.Message, g *lggraph.CompiledGraph, run uint64) iter.Seq2[lggraph.StreamChunk, error] {
	return func(yield func(lggraph.StreamChunk, error) bool) {
		a.goal.mu.Lock()
		thread := a.goal.thread
		a.goal.mu.Unlock()
		paused := false
		defer func() {
			if !paused {
				a.AbandonGoalRun(run)
			}
		}()
		for c, err := range g.Stream(ctx, a.NewState(ctx, history), lggraph.StreamOptions{
			Options: lggraph.Options{ThreadID: thread},
			Modes:   streamModes,
		}) {
			if err == nil && len(goalInterrupts(c.Mode, c.Payload)) > 0 {
				paused = true
			}
			if !yield(c, err) {
				return
			}
		}
	}
}

func (a *Agent) endGoalRun() {
	a.goal.mu.Lock()
	a.goal.running = false
	a.goal.pending = nil
	a.goal.graph = nil
	a.goal.mu.Unlock()
}

// buildGoalGraph compiles the assess → execute → evaluate loop. The recursion
// limit bounds dispatches, not cycles; each cycle costs three dispatches. The
// in-memory Saver backs the outer pauses (interrupts) with per-run threads.
func (a *Agent) buildGoalGraph(maxCycles int) *lggraph.CompiledGraph {
	g := lggraph.NewStateGraph()
	g.AddReducer("messages", messagesChannel)
	g.AddNode(goalNodeAssess, a.observedNode("goal", goalNodeAssess, a.goalAssessNode()))
	g.AddNode(goalNodeExecute, a.observedNode("goal", goalNodeExecute, a.goalExecuteNode()))
	g.AddNode(goalNodeEvaluate, a.observedNode("goal", goalNodeEvaluate, a.goalEvaluateNode()))
	g.AddEdge(goalNodeAssess, goalNodeExecute)
	g.AddEdge(goalNodeExecute, goalNodeEvaluate)
	g.AddConditionalEdges(goalNodeEvaluate, a.goalRoute)
	g.SetEntryPoint(goalNodeAssess)
	limit := maxCycles*3 + 8
	cg, err := g.Compile(
		lggraph.WithRecursionLimit(limit),
		lggraph.WithCheckpointer(lgcheckpoint.NewMemorySaver()),
	)
	if err != nil {
		// The graph is valid by construction; a compile failure is a bug.
		panic(fmt.Sprintf("agent: compile goal graph: %v", err))
	}
	return cg
}

// answerDanglingToolCalls appends synthetic tool results when the cycle was cut
// between the model and tools nodes, leaving a trailing AI message whose tool
// calls never ran. Without this the salvaged transcript is rejected by
// OpenAI-compatible providers (a tool_call message must be followed by its
// tool results before the next human message).
func answerDanglingToolCalls(msgs []messages.Message) []messages.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := msgs[len(msgs)-1]
	if last.Role != messages.RoleAI || len(last.ToolCalls) == 0 {
		return msgs
	}
	out := append([]messages.Message(nil), msgs...)
	for _, tc := range last.ToolCalls {
		out = append(out, messages.Tool(tc.ID,
			"Error: this tool call never ran — the goal cycle was cut short by the agent graph's recursion limit."))
	}
	return out
}

// isRecursionLimit reports whether err is the graph runtime's typed
// recursion (superstep) limit error (the fork's GraphRecursionError). A
// limit hit is a soft cycle boundary — the work done so far is salvaged and
// evaluated — never a hard failure of the goal loop.
func isRecursionLimit(err error) bool {
	_, ok := errors.AsType[*lgtypes.GraphRecursionError](err)
	return ok
}

// goalCycleStepBudget is the recursion (superstep) budget one goal execute
// cycle runs the inner agent graph with: double the interactive turn limit.
// Goal cycles are long autonomous runs and get a longer leash than a chat
// turn; hitting the budget is a soft cut (salvage + evaluate), so any finite
// value is safe. The per-invocation override (lggraph.Options.RecursionLimit)
// lets the one compiled agent graph serve turns and cycles at different
// budgets — with the default turn limit of 50, a cycle gets 100 supersteps
// (≈ 50 model⇄tools rounds).
func goalCycleStepBudget(turnLimit int) int {
	if turnLimit <= 0 {
		turnLimit = 50 // agent.New's default turn limit
	}
	return 2 * turnLimit
}

// goalRoute ends the loop unless the goal is still active (the evaluate node
// applies every terminal transition itself).
func (a *Agent) goalRoute(rt lgruntime.Runtime, _ map[string]any) ([]any, error) {
	a.goal.mu.Lock()
	g := a.goal.goal
	a.goal.mu.Unlock()
	if g == nil || g.Status != GoalActive {
		return []any{lgtypes.END}, nil
	}
	return []any{goalNodeAssess}, nil
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
func (a *Agent) goalAssessNode() lggraph.NodeFunc {
	return func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		ctx := rt
		emit := streamWriter(rt)
		if !a.goalBeginCycle() {
			// Cleared or paused since the last node: end the loop.
			return &lgtypes.Command{Goto: []any{lgtypes.END}}, nil
		}
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		a.goal.mu.Unlock()
		// The goal may have been cleared between goalBeginCycle and this
		// read; clone() of nil is nil, so guard like the other nodes.
		if goal == nil {
			return &lgtypes.Command{Goto: []any{lgtypes.END}}, nil
		}

		var b strings.Builder
		b.WriteString("You are the planning stage of a goal-driven agent loop.\n\n")
		fmt.Fprintf(&b, "Goal: %s\n", goal.Objective)
		fmt.Fprintf(&b, "Cycle: %d of at most %d · tokens used: %s of %s\n\n", goal.Cycles, goal.MaxCycles, formatTokens(goal.TokensUsed), formatTokens(goal.MaxTokens))
		if len(goal.Progress) == 0 {
			b.WriteString("Progress so far: none yet — this is the first cycle. Start by proposing 3-6 concrete, verifiable done-criteria for the goal as a markdown checklist, then plan the first actions.\n\n")
		} else {
			b.WriteString("Progress so far (most recent last):\n")
			start := max(len(goal.Progress)-5, 0)
			for _, p := range goal.Progress[start:] {
				fmt.Fprintf(&b, "  cycle %d [%s] %s\n", p.Cycle, p.Verdict, p.Summary)
			}
			if len(goal.Notes) > 0 {
				b.WriteString("Executor notes:\n")
				notes := goal.Notes
				if len(notes) > 3 {
					notes = notes[len(notes)-3:]
				}
				for _, n := range notes {
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
		emit(GoalNote{Text: fmt.Sprintf("\n[goal cycle %d/%d] %s\n", goal.Cycles, goal.MaxCycles, goalTruncate(firstLine(assessment), 200))})
		return map[string]any{"assessment": assessment}, nil
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
// pauses are surfaced to the harness through the outer graph's Interrupt
// (same payload); the inner thread is stashed so a resume feeds the answer
// into the inner graph's resume.
func (a *Agent) goalExecuteNode() lggraph.NodeFunc {
	return func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		ctx := rt
		emit := streamWriter(rt)
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		pending := a.goal.pending
		pendingThread := a.goal.innerThread
		pendingTurns := a.goal.pendingTurns
		pendingPrefix := a.goal.pendingPrefix
		a.goal.mu.Unlock()
		if goal == nil || goal.Status != GoalActive {
			return &lgtypes.Command{Goto: []any{lgtypes.END}}, nil
		}
		assessment, _ := state["assessment"].(string)

		msgs := MessagesFrom(state)
		hist := msgs
		if len(hist) > 0 && hist[0].Role == messages.RoleSystem {
			hist = hist[1:]
		}

		var stream iter.Seq2[lggraph.StreamChunk, error]
		var prefix int
		var turnsBefore int
		budget := goalCycleStepBudget(a.recursionLimit)
		if pending != nil {
			// Resume path: Interrupt returns the harness's answer on this
			// re-entry (it paused the outer graph on the previous pass),
			// which feeds the inner graph's resume. The cycle prompt and
			// prefix are taken from the stash — they must not be rebuilt
			// (compaction could re-summarize differently).
			answer := lggraph.Interrupt(ctx, pending.Value)
			stream = a.resumeStreamWithBudget(ctx, pendingThread, answer, budget)
			prefix = pendingPrefix
			turnsBefore = pendingTurns
			a.goal.setPending(nil, "", 0, 0)
		} else {
			hist = a.CompactHistory(ctx, hist)
			cycleMsg := goalCycleMsg(goal.Cycles, goal.MaxCycles, goal.Objective, assessment, goal.TokensUsed, goal.MaxTokens)
			prefix = len(hist)
			pendingThread = NewThreadID()
			stream = a.streamWithBudget(ctx, append(hist, cycleMsg), pendingThread, budget)
			a.ResetTurnCounter()
			turnsBefore = 0
		}

		var newMsgs []messages.Message
		var lastValues map[string]any
		limitHit := false
		pump := func() error {
			for chunk, err := range stream {
				if err != nil {
					if isRecursionLimit(err) {
						// The cycle outgrew the inner graph's dispatch budget
						// (long autonomous cycles legitimately run dozens of
						// model⇄tools rounds). Salvage the work completed so
						// far, mark the cut, and hand the cycle to the
						// evaluator instead of killing the whole goal loop —
						// the next cycle starts with a fresh budget.
						limitHit = true
						return nil
					}
					return err
				}
				if intrs := goalInterrupts(chunk.Mode, chunk.Payload); len(intrs) > 0 {
					// Stash the inner pause and surface it on the outer
					// graph: Interrupt panics GraphInterrupt on this pass
					// (pausing the goal loop) and returns the harness's
					// answer on the resumed pass.
					inner := intrs[0]
					a.goal.setPending(&inner, pendingThread, turnsBefore, prefix)
					lggraph.Interrupt(ctx, inner.Value)
					return nil // unreachable: Interrupt panics
				}
				switch chunk.Mode {
				case lggraph.StreamCustom:
					if s, ok := chunk.Payload.(string); ok && s != "" {
						// The inner agent's answer text and restart markers are
						// loop narration, not chat replies — re-emit typed so
						// renderers can keep them out of the answer stream.
						emit(GoalNote{Text: s})
						continue
					}
					if rd, ok := chunk.Payload.(ReasoningDelta); ok && rd.Text != "" {
						emit(rd)
					}
				case lggraph.StreamUpdates:
					updates, _ := chunk.Payload.(map[string]any)
					for _, v := range updates {
						nodeUpdate, _ := v.(map[string]any)
						for _, m := range MessagesFrom(nodeUpdate) {
							if m.Role == messages.RoleAI && len(m.ToolCalls) > 0 || m.Role == messages.RoleTool {
								emit(GoalDelta{Message: m})
							}
						}
					}
				case lggraph.StreamValues:
					lastValues = valuesState(chunk.Payload)
				}
			}
			// A clean end-of-stream with a values snapshot is a completed
			// run; without one the run ended abnormally.
			if lastValues == nil && !limitHit {
				return fmt.Errorf("agent: goal execute stream ended without a terminal state")
			}
			return nil
		}
		if err := pump(); err != nil {
			return nil, err
		}
		if lastValues != nil {
			final := MessagesFrom(lastValues)
			// final = [system] + prefix history + this cycle's messages.
			if len(final) > prefix+1 {
				newMsgs = final[prefix+1:]
			}
		}
		if limitHit {
			newMsgs = answerDanglingToolCalls(newMsgs)
			cutNotice := "\n[goal cycle cut short: agent-graph recursion limit reached — salvaging this cycle's work for evaluation]\n"
			emit(GoalNote{Text: cutNotice})
			newMsgs = append(newMsgs, messages.AI(strings.TrimSpace(cutNotice)))
		}
		cycleTurns := a.TurnsDone() - turnsBefore
		return map[string]any{
			"messages":    newMsgs,
			"cycle_turns": cycleTurns,
		}, nil
	}
}

// goalEvaluateNode is the verification gate: a skeptical evaluator reviews the
// cycle's evidence and moves the goal state machine. Only a verified verdict
// completes the goal; claims alone never do.
func (a *Agent) goalEvaluateNode() lggraph.NodeFunc {
	return func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		ctx := rt
		emit := streamWriter(rt)
		a.goal.mu.Lock()
		goal := a.goal.goal.clone()
		a.goal.mu.Unlock()
		if goal == nil {
			return &lgtypes.Command{Goto: []any{lgtypes.END}}, nil
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

		// Structured output (langchain v0.6): the verdict schema goes
		// upstream when the model enforces JSON schemas natively; the
		// router degrades models without the capability to a plain invoke,
		// where the prompt-guided JSON reply and the lenient parse below
		// stay authoritative.
		verdictInput := []messages.Message{messages.Human(b.String())}
		var resp messages.Message
		var err error
		if sc, ok := a.goalModel().(language.StructuredCaller); ok {
			resp, err = sc.InvokeStructured(ctx, verdictInput, goalVerdictSchema())
		} else {
			resp, err = a.goalModel().Invoke(ctx, verdictInput)
		}
		if err != nil {
			return nil, err
		}
		a.usage.recordUsage(resp.UsageMetadata)
		v := parseGoalVerdict(resp.Content)
		emit(GoalNote{Text: fmt.Sprintf("\n[goal verdict: %s] %s\n", v.Verdict, goalTruncate(orString(v.Evidence, v.Gaps), 200))})

		a.goal.mu.Lock()
		g := a.goal.goal
		if g == nil {
			a.goal.mu.Unlock()
			return &lgtypes.Command{Goto: []any{lgtypes.END}}, nil
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
		// wins over the verdict. A cleared goal reads as "" here, which
		// falls into the same already-terminal arm.
		a.goal.mu.Lock()
		var status GoalStatus
		if a.goal.goal != nil {
			status = a.goal.goal.Status
		}
		a.goal.mu.Unlock()
		switch {
		case status != GoalActive:
			// already paused/blocked/cleared: report and stop
		case v.Verdict == verdictComplete:
			a.goalSetStatus(GoalComplete, "", orString(v.Evidence, "verified by evaluator"))
		case v.Verdict == verdictBlocked:
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
		var finalStatus GoalStatus
		if a.goal.goal != nil {
			finalStatus = a.goal.goal.Status
		}
		a.goal.mu.Unlock()
		if finalStatus == "" {
			// Cleared while evaluating: nothing to report against.
			return map[string]any{"verdict": v.Verdict}, nil
		}
		if finalStatus != GoalActive {
			report := goalReport(finalStatus, v, goal.Cycles)
			unified := goalUnifiedReply(goalCycleMessages(MessagesFrom(state)), report)
			// One unified chat reply for the whole run; the persisted twin is
			// wrapped so transcript reloads can tell the report apart from
			// intermediate cycle chatter (rendered as loop progress, not as
			// chat bubbles).
			emit(GoalFinalReply{Text: unified})
			return map[string]any{
				"messages": []messages.Message{messages.AI(GoalReportWrap(unified))},
				"verdict":  v.Verdict,
			}, nil
		}
		return map[string]any{"verdict": v.Verdict}, nil
	}
}

// GoalReportWrap marks a persisted AI message as the goal run's unified final
// reply. Display layers strip the markers and render the content as the
// single assistant bubble of the run.
func GoalReportWrap(text string) string {
	return "<goal-report>\n" + text + "\n</goal-report>"
}

// goalUnifiedReply composes the run's single chat reply: the executor's last
// substantive answer of the final cycle (for research-style goals it is the
// actual deliverable, not the status footer) followed by the terminal report.
func goalUnifiedReply(cycleMsgs []messages.Message, report string) string {
	last := ""
	for _, m := range cycleMsgs {
		if m.Role == messages.RoleAI && strings.TrimSpace(m.Content) != "" {
			last = m.Content
		}
	}
	last = strings.TrimSpace(last)
	if last == "" || strings.Contains(report, last) {
		return report
	}
	return last + "\n\n---\n\n" + report
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
		used := max(total-base, 0)
		a.goal.goal.TokensUsed = used
	}
	a.goal.mu.Unlock()
}

func (m *goalManager) setPending(ie *lgtypes.Interrupt, innerThread string, turns, prefix int) {
	m.mu.Lock()
	m.pending = ie
	m.innerThread = innerThread
	m.pendingTurns = turns
	m.pendingPrefix = prefix
	m.mu.Unlock()
}

// goalVerdictSchema is the evaluator's structured-output contract
// (langchain v0.6 InvokeStructured): providers with native JSON-schema
// enforcement get it upstream; the rest are validated against it.
func goalVerdictSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{
		"verdict":    schema.String("complete | continue | blocked"),
		"confidence": schema.Number("confidence in the verdict, 0.0 to 1.0"),
		"evidence":   schema.String("what was concretely verified"),
		"gaps":       schema.String("what is missing (for continue)"),
		"next_step":  schema.String("the single most valuable next action"),
		// Only verdict is required — it alone drives the state machine.
		// Everything else is best-effort narration, matching the lenient
		// parse below (missing fields render empty).
	}, "verdict")
}

// goalVerdict is the evaluator's structured output.
// Goal-verdict tokens shared by the evaluate prompt, the state machine and
// the response parser. They appear verbatim in the model prompt, so changing
// a value means changing the prompt text too.
const (
	verdictComplete = "complete"
	verdictContinue = "continue"
	verdictBlocked  = "blocked"
)

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
	v := goalVerdict{Verdict: verdictContinue}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		v.Evidence = goalTruncate(s, 300)
		return v
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		v = goalVerdict{Verdict: verdictContinue, Evidence: goalTruncate(s, 300)}
		return v
	}
	switch v.Verdict {
	case verdictComplete, verdictContinue, verdictBlocked:
	default:
		v.Verdict = verdictContinue
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
	for i, msg := range slices.Backward(msgs) {
		if msg.Role == messages.RoleHuman && strings.HasPrefix(msg.Content, "<goal-cycle") {
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
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

func orString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
