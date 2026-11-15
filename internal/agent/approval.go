package agent

// The hooks and approval supersteps (graph-native HITL design §3.1, audit
// fix C1). Between the model and the tools node, TWO dedicated supersteps
// replace the old tools-node gate loop:
//
//   - `hooks` (sequential, NEVER interrupts): speculative-future skip,
//     in-process guards, the interaction-tool switch (AskUserQuestion /
//     EnterPlanMode / ExitPlanMode), the pure rule/grant/mode evaluation
//     (evalPermission), PermissionRequest + Notification hooks and PreToolUse
//     hooks. It writes the `decisions` channel — one callDecision per call of
//     the batch. Because it never interrupts, it never re-executes: resume
//     replays only the interrupted node, so every hook here fires exactly
//     once per model round (the R4/R5 fix).
//   - `approval`: collects every ask decision (permission asks, guard
//     escalations, AskUserQuestion, plan approval) into ONE interrupt
//     (ApprovalRequest), merges the ApprovalAnswer's per-call outcomes back
//     into `decisions`, and applies "allow for session" grants. Its
//     pre-interrupt work is pure list building, so its resume re-execution is
//     harmless; the post-answer work (grants, plan-flag flip) runs exactly
//     once, after the answer arrives.
//
// The tools node then executes purely from `decisions` — no gate logic, no
// gate interrupts. Decisions persist in graph state, so a mode switch or a
// plan approval while parked cannot shift an asked call's outcome (N2a/N2b).

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// Node names of the two new supersteps (graph-build constants live in
// agent.go; these are the persisted checkpoint names — stable across
// releases).
const (
	nodeHooks    = "hooks"
	nodeApproval = "approval"

	// decisionsChannelName is the graph state channel carrying the current
	// batch's gate decisions (LastValue semantics: each hooks round replaces
	// the whole table, so stale entries never leak into a later round).
	decisionsChannelName = "decisions"

	// OriginMain tags asks raised by the main agent graph (Task 7 adds
	// subagent origins).
	OriginMain = "main"
)

// Ask outcome / grant constants for AskDecision.
const (
	OutcomeAllow = "allow"
	OutcomeDeny  = "deny"

	GrantNone    = "none"
	GrantSession = "session"
)

// Ask is one approval request inside the single-interrupt envelope. The
// brief's minimal shape (CallID/ToolName/ArgsSummary/Reason/Origin/
// SessionRuleKey) is extended with ToolArgs (the legacy web card and the
// Task relay's inner PermissionRequest need the structured args, not just a
// summary), Questions (an AskUserQuestion ask's structured questions) and
// Plan (an ExitPlanMode ask's plan text) — all optional, JSON-tagged, and
// omitted for plain permission asks.
type Ask struct {
	CallID         string           `json:"call_id"`
	ToolName       string           `json:"tool_name"`
	ArgsSummary    string           `json:"args_summary,omitempty"`
	ToolArgs       map[string]any   `json:"tool_args,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Origin         string           `json:"origin"`
	SessionRuleKey string           `json:"session_rule_key,omitempty"`
	Questions      []tools.Question `json:"questions,omitempty"`
	Plan           string           `json:"plan,omitempty"`
}

// ApprovalRequest is the ONE interrupt the approval node raises per round:
// every ask the batch produced, answered together by one ApprovalAnswer.
type ApprovalRequest struct {
	Asks []Ask `json:"asks"`
}

// AskDecision is the answer for one Ask.
type AskDecision struct {
	// Outcome is "allow" or "deny".
	Outcome string `json:"outcome"`
	// Grant is "none" or "session" (an allow may persist its rule key for
	// the session; denies never grant).
	Grant string `json:"grant,omitempty"`
	// Answers carries the per-question answers of an AskUserQuestion ask.
	Answers []string `json:"answers,omitempty"`
}

// ApprovalAnswer is the resume value answering an ApprovalRequest: one
// decision per asked call ID.
type ApprovalAnswer struct {
	Decisions map[string]AskDecision `json:"decisions"`
}

// Ask kinds (askInfo.Kind).
const (
	askPermission = "permission"
	askUser       = "ask_user"
	askPlan       = "plan"
)

// askInfo marks a callDecision as waiting for the user, and what kind of
// ask it is. Guard records a guard-escalated ask: its denial keeps the plain
// denial text (the guard's verdict was the user's, mirroring the pre-Task-5
// run[i]=false path).
type askInfo struct {
	Kind  string `json:"kind"`
	Guard bool   `json:"guard,omitempty"`
}

// callDecision is the committed gate decision for one tool call. Exactly one
// of the terminal shapes holds:
//   - Ask != nil: waiting for the approval node (merged there).
//   - Override != nil: do not execute; the override is the tool result.
//   - Run: execute (PreFeedback is prepended to the result).
type callDecision struct {
	Run         bool              `json:"run"`
	Override    *coretools.Result `json:"override,omitempty"`
	PreFeedback string            `json:"pre_feedback,omitempty"`
	Ask         *askInfo          `json:"ask,omitempty"`
}

// decisionsState is the `decisions` channel value: a struct wrapper so the
// durable checkpoint serde can envelope it (cpserde registers struct types
// only; a bare map[string]callDecision would fail durable savers).
type decisionsState struct {
	PerCall map[string]callDecision `json:"per_call"`
}

// decisionsChannel is the LastValue reducer for the decisions channel.
func decisionsChannel(existing, update any) (any, error) {
	if update == nil {
		return existing, nil
	}
	return update, nil
}

// decisionsFrom extracts the decisions table from a graph state (nil-safe).
func decisionsFrom(state map[string]any) decisionsState {
	if state == nil {
		return decisionsState{}
	}
	ds, _ := state[decisionsChannelName].(decisionsState)
	return ds
}

// legacyInterruptedMarker is the tool result synthesized for calls resumed
// from a legacy (pre-decisions) checkpoint (spec §3.5): the tool may or may
// not have run in the old process — never re-execute it blindly.
const legacyInterruptedMarker = "Error: interrupted (pre-upgrade checkpoint); the tool may or may not have run — verify before retrying."

// argsSummary renders a compact one-line summary of a call's arguments for
// the ask envelope (full args ride in Ask.ToolArgs).
func argsSummary(call messages.ToolCall) string {
	if len(call.Args) == 0 {
		return ""
	}
	blob, err := json.Marshal(call.Args)
	if err != nil {
		return ""
	}
	s := string(blob)
	if len(s) > 480 {
		s = s[:480] + "…"
	}
	return s
}

// hooksNode derives the batch's gate decisions. Sequential, NEVER
// interrupts; all side effects (plan-flag writes, hook commands,
// notifications) happen exactly once because the node never re-executes.
func (a *Agent) hooksNode() lggraph.NodeFunc {
	return func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		ctx := rt
		msgs := MessagesFrom(state)
		if len(msgs) == 0 {
			return nil, nil
		}
		last := msgs[len(msgs)-1]
		if last.Role != messages.RoleAI || len(last.ToolCalls) == 0 {
			return nil, nil
		}
		calls := last.ToolCalls
		specRun := specRunID(ctx)
		out := make(map[string]callDecision, len(calls))
		for _, call := range calls {
			d, err := a.hooksDecide(ctx, specRun, call)
			if err != nil {
				return nil, err
			}
			out[call.ID] = d
		}
		// LastValue semantics: the fresh table REPLACES the previous round's
		// (stale decisions never leak across rounds — the map is rebuilt,
		// not merged).
		return map[string]any{decisionsChannelName: decisionsState{PerCall: out}}, nil
	}
}

// hooksDecide derives one call's decision: spec-future skip, guards, the
// interaction-tool switch, the pure permission evaluation, then the
// PreToolUse hook for calls that will run.
func (a *Agent) hooksDecide(ctx context.Context, specRun string, call messages.ToolCall) (callDecision, error) {
	// Speculative futures already ran their gate (dispatchSpeculative's
	// speculativeAllowed) and their PreToolUse hook (runToolCall); the tools
	// node only awaits the result.
	if a.spec.get(specRun, call.ID) != nil {
		return callDecision{Run: true}, nil
	}
	d, err := a.hooksGateWithGuards(ctx, call)
	if err != nil {
		return callDecision{}, err
	}
	if d.Ask != nil || d.Override != nil || !d.Run {
		return d, nil
	}
	// The call will execute: run its PreToolUse hook now (once — this node
	// never re-executes). A hook error fails the round, mirroring the old
	// tools-node propagation.
	if h := a.hooksFor(ctx); h != nil {
		dec, err := h.PreToolUse(ctx, call.Name, call.Args)
		if err != nil {
			return callDecision{}, err
		}
		if !dec.Allowed {
			content := "Error: tool use denied by hook"
			if dec.Feedback != "" {
				content = "Error: " + dec.Feedback
			}
			return callDecision{Override: &coretools.Result{Content: content}}, nil
		}
		d.PreFeedback = dec.Feedback
	}
	return d, nil
}

// hooksGateWithGuards runs the in-process guards, then the gate: a guard
// deny overrides with the guard's reason; a guard ask escalates to the user
// (the verdict is FINAL, Task 1 — a denial must not fall through to the
// rules/mode arms, an approval already granted what they would ask); no
// guard verdict proceeds to the normal gate.
func (a *Agent) hooksGateWithGuards(ctx context.Context, call messages.ToolCall) (callDecision, error) {
	if len(a.toolGuards) > 0 {
		switch g := a.guardsBeforeTool(ctx, call); g.Action {
		case ToolDeny:
			content := "Error: tool use denied by guard"
			if g.Reason != "" {
				content = "Error: " + g.Reason
			}
			return callDecision{Override: &coretools.Result{Content: content}}, nil
		case ToolAsk:
			return a.hooksAsk(ctx, call, true), nil
		}
	}
	return a.hooksGate(ctx, call)
}

// hooksGate is the non-interrupting successor of the old toolsNode gate: the
// interaction-tool switch plus the pure permission evaluation. Paths that
// used to lggraph.Interrupt now mark the call as an ask for the approval
// node; PermissionRequest hooks may still pre-decide an ask (their decision
// replaces the user round-trip), and the Notification hook fires exactly
// once per surfaced ask, here (never in the approval node, which re-executes
// on resume).
func (a *Agent) hooksGate(ctx context.Context, call messages.ToolCall) (callDecision, error) {
	switch call.Name {
	case "AskUserQuestion":
		var q struct {
			Questions []tools.Question `json:"questions"`
		}
		if err := decodeArgs(call.Args, &q); err != nil {
			// Malformed ask arguments fail the round loudly (the old gate
			// propagated the decode error out of the tools node).
			return callDecision{}, err
		}
		return callDecision{Ask: &askInfo{Kind: askUser}}, nil

	case "EnterPlanMode":
		a.setPlanModeFor(ctx, true)
		return callDecision{Override: &coretools.Result{Content: "Plan mode enabled. Use read-only tools to research and write a plan, then call ExitPlanMode to request approval before implementing."}}, nil

	case "ExitPlanMode":
		if !a.planModeFor(ctx) {
			return callDecision{Override: &coretools.Result{Content: "Not currently in plan mode."}}, nil
		}
		var p struct {
			Plan string `json:"plan"`
		}
		if err := decodeArgs(call.Args, &p); err != nil {
			return callDecision{}, err
		}
		return callDecision{Ask: &askInfo{Kind: askPlan}}, nil
	}

	allow, ask := a.evalPermission(ctx, call)
	switch {
	case ask:
		return a.hooksAsk(ctx, call, false), nil
	case allow:
		return callDecision{Run: true}, nil
	default:
		return callDecision{Override: &coretools.Result{Content: "Error: permission denied by user"}}, nil
	}
}

// hooksAsk runs the ask-side effects that must happen exactly once — the
// PermissionRequest hook (allow/deny pre-decisions) and, when the ask will
// actually surface, the Notification hook — and returns the ask-marked (or
// pre-decided) decision. guard marks a guard-escalated ask.
func (a *Agent) hooksAsk(ctx context.Context, call messages.ToolCall, guard bool) callDecision {
	if h := a.hooksFor(ctx); h != nil {
		if d, err := h.PermissionRequest(ctx, call.Name, call.Args); err == nil && d.Decision != "" {
			if d.Decision == "allow" {
				return callDecision{Run: true}
			}
			if guard {
				// Guard-escalated denials keep the plain denial text (the
				// pre-Task-5 run[i]=false path).
				return callDecision{}
			}
			return callDecision{Override: &coretools.Result{Content: "Error: permission denied by user"}}
		}
	}
	if h := a.hooksFor(ctx); h != nil {
		_, _ = h.Notification(ctx, "permission required: "+call.Name)
	}
	return callDecision{Ask: &askInfo{Kind: askPermission, Guard: guard}}
}

// approvalNode surfaces every ask of the batch as ONE interrupt and merges
// the answers into the decisions channel. Pure list-building before the
// interrupt; grants and the plan-flag flip run only after the answer arrives
// (exactly once, even though the node re-executes on resume).
func (a *Agent) approvalNode() lggraph.NodeFunc {
	return func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		ctx := rt
		msgs := MessagesFrom(state)
		if len(msgs) == 0 {
			return nil, nil
		}
		last := msgs[len(msgs)-1]
		if last.Role != messages.RoleAI || len(last.ToolCalls) == 0 {
			return nil, nil
		}
		ds := decisionsFrom(state)
		var asks []Ask
		for _, call := range last.ToolCalls {
			d, ok := ds.PerCall[call.ID]
			if !ok || d.Ask == nil {
				continue
			}
			ask := Ask{
				CallID:         call.ID,
				ToolName:       call.Name,
				ArgsSummary:    argsSummary(call),
				ToolArgs:       call.Args,
				Origin:         OriginMain,
				SessionRuleKey: GrantKey(call),
			}
			switch d.Ask.Kind {
			case askPermission:
				ask.Reason = toolReason(call)
			case askPlan:
				var p struct {
					Plan string `json:"plan"`
				}
				_ = decodeArgs(call.Args, &p) // validated by the hooks node
				ask.Plan = p.Plan
				ask.Reason = "Approve the plan to proceed with implementation."
			case askUser:
				var q struct {
					Questions []tools.Question `json:"questions"`
				}
				_ = decodeArgs(call.Args, &q) // validated by the hooks node
				ask.Questions = q.Questions
				ask.Reason = "The agent has a question for you."
			}
			asks = append(asks, ask)
		}
		if len(asks) == 0 {
			return nil, nil // nothing to ask: straight through to the tools
		}
		answer := decodeResumeValue(decodeInterruptPayload(lggraph.Interrupt(ctx, encodeInterruptPayload(ApprovalRequest{Asks: asks}))))
		merged := decisionsState{PerCall: maps.Clone(ds.PerCall)}
		if merged.PerCall == nil {
			merged.PerCall = map[string]callDecision{}
		}
		for _, ask := range asks {
			d := merged.PerCall[ask.CallID]
			kind, guard := d.Ask.Kind, d.Ask.Guard
			d.Ask = nil
			dec := decisionFor(answer, ask)
			allowed := dec.Outcome == OutcomeAllow
			switch kind {
			case askPermission:
				if allowed {
					d.Run = true
					if dec.Grant == GrantSession {
						a.applySessionGrant(ctx, ask.SessionRuleKey)
					}
				} else if guard {
					d.Run = false // plain denial text, guard-escalated ask
				} else {
					d.Override = &coretools.Result{Content: "Error: permission denied by user"}
				}
			case askPlan:
				if allowed {
					a.setPlanModeFor(ctx, false)
					d.Override = &coretools.Result{Content: "Plan approved by the user. Proceeding with implementation."}
				} else {
					d.Override = &coretools.Result{Content: "Plan rejected by the user. Revise your plan based on their feedback and call ExitPlanMode again."}
				}
			case askUser:
				d.Override = &coretools.Result{Content: formatAskUserAnswers(ask.Questions, dec.Answers)}
			}
			merged.PerCall[ask.CallID] = d
		}
		return map[string]any{decisionsChannelName: merged}, nil
	}
}

// decisionFor resolves the answer value for one ask: the envelope's
// per-call decision when the driver sent an ApprovalAnswer; the legacy
// AskUserAnswer shape mapped onto the (single) ask_user ask; any other
// (scalar) value — the single-ask backward-compat resume old clients and
// scalar-answer scripts still send — applied to the ask via parseAnswer.
// Anything unrecognized fails closed (deny).
func decisionFor(answer any, ask Ask) AskDecision {
	switch ans := answer.(type) {
	case ApprovalAnswer:
		if d, ok := ans.Decisions[ask.CallID]; ok {
			return d
		}
		return AskDecision{Outcome: OutcomeDeny}
	case AskUserAnswer:
		if ask.ToolName == "AskUserQuestion" {
			return AskDecision{Outcome: OutcomeAllow, Answers: ans.Answers}
		}
		return AskDecision{Outcome: OutcomeDeny}
	}
	if parseAnswer(answer) {
		return AskDecision{Outcome: OutcomeAllow}
	}
	return AskDecision{Outcome: OutcomeDeny}
}

// applySessionGrant records an "allow for session" grant through the run's
// policy source: the ctx-carried per-session store when the run carries one
// (serve mode), the agent's source otherwise (TUI/CLI/tests single-session
// surface).
func (a *Agent) applySessionGrant(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if session, src, ok := SessionPolicyFrom(ctx); ok {
		src.AddGrant(session, key)
		return
	}
	a.PolicySource().AddGrant("", key)
}

// routeTools is route2: everything flows to the tools node as pre-decided
// entries. Task 7 fans foreground Task calls out via types.Send here.
func (a *Agent) routeTools(rt lgruntime.Runtime, state map[string]any) ([]any, error) {
	return []any{nodeTools}, nil
}

// evalPermission is the pure permission evaluation shared by the hooks node
// and the legacy checkPermission seam: explicit rules (deny/allow/ask), then
// session grants (never while plan is enforced), then the mode arms with the
// plan override. It never interrupts and runs no hooks.
func (a *Agent) evalPermission(ctx context.Context, call messages.ToolCall) (allow, ask bool) {
	if a.perms != nil {
		if action, ok := a.perms.Action(call.Name, call.Args); ok {
			switch action {
			case "deny":
				return false, false
			case "allow":
				return true, false
			default: // ask
				return false, true
			}
		}
	}
	p := a.policyFor(ctx)
	readOnly := tools.IsReadOnly(call.Name, call.Args)
	cat := tools.CategoryOf(call.Name)
	mode := p.Mode
	if p.Plan {
		// Plan mode is enforced structurally, whatever the base mode (the
		// plan override outranks even bypass; see TestPlanBeatsBypassDirect).
		mode = config.ModePlan
	}
	// Session grants sit strictly between the explicit rules and the mode
	// arms: a deny or ask rule above has already returned, and enforced plan
	// beats grants, so a grant can only lift the mode-arm ask.
	if !p.Plan {
		if key := GrantKey(call); key != "" && p.Grants[key] {
			return true, false
		}
	}
	switch mode {
	case config.ModeBypass:
		return true, false
	case config.ModePlan, config.ModeReadOnly:
		return readOnly || cat == tools.CatMeta, false
	case config.ModeAcceptEdits:
		if cat == tools.CatWrite {
			return true, false
		}
		if readOnly || cat == tools.CatMeta {
			return true, false
		}
		return false, true
	default: // ModeDefault
		if readOnly || cat == tools.CatMeta {
			return true, false
		}
		return false, true
	}
}

// hasPendingAsk reports whether the decisions table still carries an
// unresolved ask (defensive: the approval node merges every ask before the
// tools node runs; a survivor means corrupted state).
func (ds decisionsState) hasPendingAsk(calls []messages.ToolCall) bool {
	for _, call := range calls {
		if d, ok := ds.PerCall[call.ID]; ok && d.Ask != nil {
			return true
		}
	}
	return false
}

// missingDecision reports whether any call of the batch has no committed
// decision — impossible in the new topology (the hooks node writes one
// entry per call), so its presence means the checkpoint predates the
// decisions channel: a legacy relay-era pause.
func missingDecision(ds decisionsState, calls []messages.ToolCall) bool {
	if len(ds.PerCall) == 0 {
		return true
	}
	for _, call := range calls {
		if _, ok := ds.PerCall[call.ID]; !ok {
			return true
		}
	}
	return false
}

// legacyMarkers synthesizes interrupted tool results for a legacy batch.
func legacyMarkers(calls []messages.ToolCall) []messages.Message {
	out := make([]messages.Message, len(calls))
	for i, call := range calls {
		out[i] = messages.Tool(call.ID, legacyInterruptedMarker)
	}
	return out
}
