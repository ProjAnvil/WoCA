package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// Event is one SSE frame. Seq is a per-run monotonic sequence (from 1)
// assigned by emit; it is written as the SSE id: line for Last-Event-ID
// resume and never serialized into Data. It aliases the platform eventbus
// event type so server events flow over the shared Bus interface.
type Event = eventbus.Event

// subscriber receives live events for a single SSE connection.
type subscriber struct {
	ch chan Event
}

// parkedCard is one pending interrupt card of a parked pause: its answer
// key (NS, or ID for legacy NS-less interrupts), its wire kind
// (permission/ask/plan/approvals), its rendered payload, and — for
// approvals cards — the envelope's asks so the answer path can translate
// legacy {approve} bodies into per-ask ApprovalAnswers.
type parkedCard struct {
	id      string
	ns      string
	kind    string
	payload any
	asks    []agent.Ask
}

// key is the answer-map key addressing this card.
func (c parkedCard) key() string {
	if c.ns != "" {
		return c.ns
	}
	return c.id
}

// interruptState holds a parked graph pause awaiting an HTTP answer: every
// pending card of the pause (one per interrupt — the main approval
// envelope and/or each paused child), answered together.
type interruptState struct {
	cards []parkedCard
	reply chan any
}

// Run is one in-flight agent execution.
type Run struct {
	id     string
	a      *agent.Agent
	ctx    context.Context
	cancel context.CancelFunc
	// thread is the checkpoint thread id the runner will pin (minted by the
	// server so the externalized run ledger records the same value).
	thread string

	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	history []Event
	pending *interruptState
	done    bool // terminal event (done or error) emitted
	// finishKind records the terminal event type for the run ledger.
	finishKind string
	// finishCancelled, under mu, records whether the terminal done event was
	// a cancellation. The wake driver skips cancelled (and errored) runs
	// when deciding whether to re-check the session's queue — see ranClean.
	finishCancelled bool
	// mirror, when set, persists every emitted event to the run ledger's
	// run_events stream (cross-instance SSE replay). Failures are the
	// caller's to log; they never block the live stream.
	mirror  func(ev Event)
	seq     int64        // under mu; last assigned Seq
	dropped atomic.Int64 // fanout drops on full subscriber channels
	// onFinish, when set, fires exactly once when the terminal event is
	// emitted — on error and cancellation too, before onDone runs.
	onFinish func()
	// onPark / onResume notify the owner of interrupt transitions so the
	// session run state can flip to awaiting and back. Nil on runs that
	// never record state (command runs, bare tests).
	onPark   func()
	onResume func()
}

func newRun(id string, a *agent.Agent, ctx context.Context, cancel context.CancelFunc) *Run {
	return &Run{
		id:     id,
		a:      a,
		ctx:    ctx,
		cancel: cancel,
		subs:   map[*subscriber]struct{}{},
	}
}

// ID returns the run identifier.
func (r *Run) ID() string { return r.id }

// Session returns the session id this run is scoped to — the run scope its
// ctx carries (set by newRunContext before the run exists, so it is stable
// for the Run's lifetime); "" for a run without one. The steer endpoint
// uses it to scope queued input to the session the user is looking at.
func (r *Run) Session() string {
	if sc, ok := tracing.RunFrom(r.ctx); ok {
		return sc.SessionID
	}
	return ""
}

// start runs the agent loop in a goroutine. onDone fires exactly once on
// every exit path; final is nil when the run ended with an error or cancel
// (nothing to persist), which lets callers hook post-run cleanup (session
// gate release) to a single point.
func (r *Run) start(history []messages.Message, onDone func([]messages.Message)) {
	r.startWith(func() ([]messages.Message, error) {
		return runner.New(r.a, r.handleInterrupt, r.render).WithThreadID(r.thread).Run(r.ctx, history)
	}, onDone)
}

// startGoal launches the goal loop (assess → execute → evaluate cycles) as a
// run, streaming its progress through the same SSE event vocabulary plus
// "goal" events pushed by the server on every goal-state change. onDone
// fires exactly once on every exit path; final is nil on error or cancel.
func (r *Run) startGoal(history []messages.Message, onDone func([]messages.Message)) {
	r.startWith(func() ([]messages.Message, error) {
		return runner.New(r.a, r.handleInterrupt, r.render).WithThreadID(r.thread).RunGoal(r.ctx, history)
	}, onDone)
}

// startWith runs runFn in a goroutine and routes its result through the
// shared finish/onDone path used by both start and startGoal.
func (r *Run) startWith(runFn func() ([]messages.Message, error), onDone func([]messages.Message)) {
	go func() {
		final, err := runFn()
		if err != nil {
			if r.ctx.Err() != nil {
				r.finish(EventDone, map[string]any{"cancelled": true})
			} else {
				r.finish(EventError, map[string]any{"message": err.Error()})
			}
			if onDone != nil {
				onDone(nil)
			}
			return
		}
		r.finish(EventDone, map[string]any{"cancelled": false})
		if onDone != nil {
			onDone(final)
		}
	}()
}

// render translates one stream chunk into SSE events: custom payloads carry
// token/reasoning deltas (and goal-cycle messages), update chunks carry the
// message batches nodes produced.
func (r *Run) render(chunk lggraph.StreamChunk) {
	switch chunk.Mode {
	case lggraph.StreamCustom:
		switch v := chunk.Payload.(type) {
		case string:
			if v != "" {
				r.emit(Event{Type: EventDelta, Data: map[string]any{"text": v}})
			}
		case agent.ReasoningDelta:
			if v.Text != "" {
				r.emit(Event{Type: EventReasoning, Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalNote:
			// Goal-loop narration (banners, per-cycle chatter) streams as its
			// own event type so the chat never mistakes it for the answer.
			if v.Text != "" {
				r.emit(Event{Type: EventGoalNote, Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalFinalReply:
			// The goal run's single unified reply — the one goal text that
			// reaches the chat as an answer.
			if v.Text != "" {
				r.emit(Event{Type: EventDelta, Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalDelta:
			// Messages produced inside a goal execute cycle stream through
			// the custom channel; render them like normal-run messages.
			r.renderMessage(v.Message)
		}
	case lggraph.StreamUpdates:
		updates, _ := chunk.Payload.(map[string]any)
		for _, nodeUpdate := range updates {
			state, _ := nodeUpdate.(map[string]any)
			for _, m := range agent.MessagesFrom(state) {
				r.renderMessage(m)
			}
		}
	}
}

// renderMessage emits the SSE events for one transcript message.
func (r *Run) renderMessage(m messages.Message) {
	switch m.Role {
	case messages.RoleAI:
		for _, tc := range m.ToolCalls {
			r.emit(Event{Type: EventToolCall, Data: toolCallPayload{
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Args,
			}})
		}
	case messages.RoleTool:
		r.emit(Event{Type: EventToolResult, Data: toolResultPayload{
			ToolCallID: m.ToolCallID,
			Content:    m.Content,
		}})
	}
}

// handleInterrupt classifies every card of a graph pause and parks the
// whole list awaiting one HTTP answer (design §3.4, audit fix M1): the
// interrupt event carries ALL pending cards; the answer body's per-card
// entries build the resume value — the scalar for a single pending card,
// the NS-keyed map for several (partial answers re-pause the remainder).
func (r *Run) handleInterrupt(cards []agent.PauseCard) (any, error) {
	parked := make([]parkedCard, 0, len(cards))
	for _, c := range cards {
		pc, err := classifyCard(c)
		if err != nil {
			return nil, err
		}
		parked = append(parked, pc)
	}
	reply := make(chan any, 1)
	r.mu.Lock()
	r.pending = &interruptState{cards: parked, reply: reply}
	r.mu.Unlock()
	return r.waitParked(parked, reply)
}

// classifyCard renders one pause card for the wire: its kind plus payload.
func classifyCard(c agent.PauseCard) (parkedCard, error) {
	out := parkedCard{id: c.ID, ns: c.NS}
	switch v := c.Value.(type) {
	case agent.ApprovalRequest:
		out.asks = v.Asks
		if len(v.Asks) == 1 {
			// A single-ask envelope parks under the ask's LEGACY kind with
			// the legacy flat payload — the current web InterruptModal keeps
			// rendering and answering it unchanged — plus the envelope's
			// asks for forward-compatible clients.
			ask := v.Asks[0]
			switch ask.ToolName {
			case "AskUserQuestion":
				out.kind = "ask"
				out.payload = map[string]any{"questions": ask.Questions, "asks": v.Asks}
			case "ExitPlanMode":
				out.kind = "plan"
				out.payload = map[string]any{"plan": ask.Plan, "asks": v.Asks}
			default:
				out.kind = "permission"
				out.payload = map[string]any{
					"tool_name":    ask.ToolName,
					"tool_args":    ask.ToolArgs,
					"reason":       ask.Reason,
					"is_read_only": tools.IsReadOnly(ask.ToolName, ask.ToolArgs),
					"asks":         v.Asks,
				}
			}
			return out, nil
		}
		// Multi-ask envelope: the whole envelope is the payload (Task 11's
		// HitlStack); the answer path fans legacy {approve} bodies out to
		// every ask.
		out.kind = "approvals"
		out.payload = v
		return out, nil
	case agent.PermissionRequest:
		out.kind = "permission"
		out.payload = v
		return out, nil
	case agent.SubagentInterrupt:
		// Legacy relay-era child ask: flat payload — the frontend renders the
		// permission card from tool_name/tool_args/reason; the subagent name
		// rides along.
		out.kind = "permission"
		out.payload = map[string]any{
			"subagent":     v.Subagent,
			"tool_name":    v.Inner.ToolName,
			"tool_args":    v.Inner.ToolArgs,
			"reason":       v.Inner.Reason,
			"is_read_only": v.Inner.IsReadOnly,
		}
		return out, nil
	case agent.AskUserRequest:
		out.kind = "ask"
		out.payload = v
		return out, nil
	case agent.PlanApprovalRequest:
		out.kind = "plan"
		out.payload = v
		return out, nil
	default:
		return parkedCard{}, fmt.Errorf("server: unknown interrupt value %T", c.Value)
	}
}

// parkedRunTTL bounds how long a run may sit parked on an unanswered
// interrupt before it is cancelled (local decision — no industry
// precedent; see spec §6.3).
var parkedRunTTL = 2 * time.Hour

// waitParked emits the parked pause (one card per pending interrupt) and
// blocks until an answer is posted or the run is cancelled. Unanswered
// interrupts are also bounded by parkedRunTTL: the run emits a timeout
// event and is cancelled so a forgotten ask cannot pin a session forever.
func (r *Run) waitParked(cards []parkedCard, reply chan any) (any, error) {
	kind := "multi" // several pending cards: no single top-level payload
	var payload any
	if len(cards) == 1 {
		kind = cards[0].kind
		payload = cards[0].payload
	}
	events := make([]interruptCardEvent, len(cards))
	for i, c := range cards {
		events[i] = interruptCardEvent{ID: c.id, NS: c.ns, Kind: c.kind, Payload: c.payload}
	}
	r.emit(Event{Type: EventInterrupt, Data: interruptPayload{Kind: kind, Payload: payload, Cards: events}})
	if r.onPark != nil {
		r.onPark()
	}

	timer := time.AfterFunc(parkedRunTTL, func() {
		r.emit(Event{Type: EventTimeout, Data: map[string]any{"kind": kind, "reason": "parked_ttl"}})
		r.cancelRun()
	})
	defer timer.Stop()
	select {
	case answer := <-reply:
		return answer, nil
	case <-r.ctx.Done():
		return nil, r.ctx.Err() // original cancellation-path semantics unchanged
	}
}

// cardAnswerEntry is one entry of the list-form answer body
// {answers: [{key, value}]}: key addresses one pending card (its NS, or ID
// for legacy NS-less interrupts); value is the per-kind answer object.
type cardAnswerEntry struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// decodeCardAnswerList extracts the list-form entries of an answer body
// {answers: [{key, value}]}: reporting false when "answers" is absent, an
// empty list (ambiguous with the legacy empty ask-user answer), or carries
// another shape (the legacy single-card forms — string lists, decision
// maps).
func decodeCardAnswerList(body []byte) ([]cardAnswerEntry, bool) {
	var probe struct {
		Answers json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || len(probe.Answers) == 0 {
		return nil, false
	}
	var entries []cardAnswerEntry
	if err := json.Unmarshal(probe.Answers, &entries); err != nil {
		return nil, false
	}
	if len(entries) == 0 {
		return nil, false
	}
	for _, e := range entries {
		if e.Key == "" {
			return nil, false
		}
	}
	return entries, true
}

// answer decodes an HTTP answer and resumes the parked pause. The body is
// either the list form {answers: [{key, value}]} (any card count; entries
// may answer a subset — unanswered cards re-pause), or — for a single
// pending card — the legacy flat forms ({approve}, {answers: [...]}, the
// approvals decision map).
func (r *Run) answer(body []byte) error {
	r.mu.Lock()
	p := r.pending
	r.mu.Unlock()
	if p == nil {
		return errors.New("no pending interrupt")
	}

	entries, isList := decodeCardAnswerList(body)
	var resume any
	switch {
	case isList:
		byKey := make(map[string]parkedCard, len(p.cards))
		for _, c := range p.cards {
			byKey[c.key()] = c
		}
		answered := make(map[string]any, len(entries))
		for _, e := range entries {
			card, ok := byKey[e.Key]
			if !ok {
				return fmt.Errorf("answer key %q matches no pending interrupt card", e.Key)
			}
			value, err := cardAnswerValue(card, e.Value)
			if err != nil {
				return err
			}
			answered[e.Key] = value
		}
		if len(p.cards) == 1 {
			// Single pending card: a scalar resume (keyed or not).
			resume = answered[p.cards[0].key()]
		} else {
			// NS-keyed map; cards without an entry re-pause (fork-native
			// partial semantics).
			resume = answered
		}
	case len(p.cards) == 1:
		// Legacy single-card body.
		value, err := legacySingleCardAnswer(p.cards[0], body)
		if err != nil {
			return err
		}
		resume = value
	default:
		return fmt.Errorf("decode answer: %d pending interrupt cards require {answers: [{key, value}]}", len(p.cards))
	}

	r.mu.Lock()
	if r.pending == p {
		r.pending = nil
	}
	r.mu.Unlock()

	// The resolution is part of the event history: a client replaying the
	// stream (page refresh, /api/events-driven reattach) must not resurrect
	// the interrupt card this answer just satisfied.
	kind := "multi"
	if len(p.cards) == 1 {
		kind = p.cards[0].kind
	}
	r.emit(Event{Type: EventInterruptResolved, Data: map[string]any{"kind": kind}})
	if r.onResume != nil {
		r.onResume()
	}

	select {
	case p.reply <- resume:
	default:
	}
	return nil
}

// legacySingleCardAnswer decodes the legacy flat body for a one-card park:
// ask → AskUserAnswer, approvals → ApprovalAnswer (per-ask decisions, with
// {approve} fanned out to every ask), permission/plan → the approve flag.
func legacySingleCardAnswer(card parkedCard, body []byte) (any, error) {
	switch card.kind {
	case "ask":
		var req struct {
			Answers []string `json:"answers"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("decode answer: %w", err)
		}
		return agent.AskUserAnswer{Answers: req.Answers}, nil
	case "approvals":
		var req struct {
			Approve bool `json:"approve"`
			Answers map[string]struct {
				Outcome string   `json:"outcome"`
				Grant   string   `json:"grant"`
				Answers []string `json:"answers"`
			} `json:"answers"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("decode answer: %w", err)
		}
		return approvalAnswerFromHTTP(card.asks, req.Approve, req.Answers), nil
	default: // permission, plan
		var req struct {
			Approve bool `json:"approve"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("decode answer: %w", err)
		}
		return req.Approve, nil
	}
}

// cardAnswerValue decodes one card's answer value from the list form:
// approvals cards take {decisions: {callID: ...}} or a {approve} fanout;
// ask cards take {answers: [...]}; permission/plan cards take {approve} —
// and, when the card carries the envelope's asks (single-ask envelopes park
// under the legacy kind with `asks` attached for forward-compatible
// clients), the approvals decision form too, so "allow for session" grants
// are expressible on every approval card (the flat {approve} body keeps
// its bool resume).
func cardAnswerValue(card parkedCard, raw json.RawMessage) (any, error) {
	switch card.kind {
	case "approvals":
		return decodeApprovalValue(card, raw)
	case "ask":
		var v struct {
			Answers []string `json:"answers"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decode answer value: %w", err)
		}
		return agent.AskUserAnswer{Answers: v.Answers}, nil
	default: // permission, plan
		if len(card.asks) > 0 {
			// Asks-bearing legacy card: try the decision form first; a body
			// without decisions falls through to the flat approve flag.
			if v, ok, err := decodeApprovalDecisions(card, raw); err != nil {
				return nil, err
			} else if ok {
				return v, nil
			}
		}
		var v struct {
			Approve bool `json:"approve"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decode answer value: %w", err)
		}
		return v.Approve, nil
	}
}

// decodeApprovalValue decodes the approvals-card answer form: explicit
// per-ask decisions when present, else the approve flag fanned out.
func decodeApprovalValue(card parkedCard, raw json.RawMessage) (any, error) {
	v, ok, err := decodeApprovalDecisions(card, raw)
	if err != nil {
		return nil, err
	}
	if ok {
		return v, nil
	}
	var flat struct {
		Approve bool `json:"approve"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("decode answer value: %w", err)
	}
	return approvalAnswerFromHTTP(card.asks, flat.Approve, nil), nil
}

// decodeApprovalDecisions reports the ApprovalAnswer when the body carries
// an explicit per-ask decisions map (ok=false when it does not).
func decodeApprovalDecisions(card parkedCard, raw json.RawMessage) (any, bool, error) {
	var v struct {
		Decisions map[string]struct {
			Outcome string   `json:"outcome"`
			Grant   string   `json:"grant"`
			Answers []string `json:"answers"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, fmt.Errorf("decode answer value: %w", err)
	}
	if len(v.Decisions) == 0 {
		return nil, false, nil
	}
	return approvalAnswerFromHTTP(card.asks, false, v.Decisions), true, nil
}

// approvalAnswerFromHTTP builds the ApprovalAnswer for an approvals card:
// explicit per-ask decisions when the body carries a decisions map, else
// the approve flag applied to every ask. Asks without an explicit entry
// fail closed (deny).
func approvalAnswerFromHTTP(asks []agent.Ask, approve bool, answers map[string]struct {
	Outcome string   `json:"outcome"`
	Grant   string   `json:"grant"`
	Answers []string `json:"answers"`
}) agent.ApprovalAnswer {
	out := agent.ApprovalAnswer{Decisions: make(map[string]agent.AskDecision, len(asks))}
	for _, ask := range asks {
		if a, ok := answers[ask.CallID]; ok {
			out.Decisions[ask.CallID] = agent.AskDecision{Outcome: a.Outcome, Grant: a.Grant, Answers: a.Answers}
			continue
		}
		outcome := agent.OutcomeDeny
		if approve {
			outcome = agent.OutcomeAllow
		}
		out.Decisions[ask.CallID] = agent.AskDecision{Outcome: outcome}
	}
	return out
}

// emit appends an event to history and fans it out to live subscribers.
func (r *Run) emit(ev Event) {
	r.mu.Lock()
	r.seq++
	ev.Seq = r.seq
	r.history = append(r.history, ev)
	for sub := range r.subs {
		select {
		case sub.ch <- ev:
		default:
			r.dropped.Add(1) // dropped events on slow consumers are observable (/api/stats)
		}
	}
	r.mu.Unlock()
	if r.mirror != nil {
		r.mirror(ev)
	}
}

func (r *Run) droppedCount() int64 { return r.dropped.Load() }

// finish emits the terminal event (done or error) and marks the run finished
// so the server can prune it from its registry once it ages out.
func (r *Run) finish(evType string, data map[string]any) {
	r.mu.Lock()
	first := !r.done
	r.done = true
	r.finishKind = evType
	if evType == EventDone {
		r.finishCancelled, _ = data["cancelled"].(bool)
	}
	hook := r.onFinish
	r.mu.Unlock()
	r.emit(Event{Type: evType, Data: data})
	if first && hook != nil {
		hook()
	}
}

// ranClean reports whether the run ended with a normal, non-cancelled done —
// the finish state that hands the session back to the wake driver (a clean
// turn completed, so anything still queued deserves a wake run).
func (r *Run) ranClean() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishKind == EventDone && !r.finishCancelled
}

// isDone reports whether the run already emitted its terminal event.
func (r *Run) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// subscribe registers a new SSE connection, replaying prior history first.
func (r *Run) subscribe() *subscriber {
	r.mu.Lock()
	sub := &subscriber{ch: make(chan Event, len(r.history)+64)}
	for _, ev := range r.history {
		sub.ch <- ev
	}
	r.subs[sub] = struct{}{}
	r.mu.Unlock()
	return sub
}

// subscribeSince replays only events after lastSeq, then goes live. gap is
// true when lastSeq points before the oldest retained event (the replay
// window can't cover it — the client should refetch a full snapshot).
func (r *Run) subscribeSince(lastSeq int64) (*subscriber, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := &subscriber{ch: make(chan Event, len(r.history)+64)}
	gap := false
	if lastSeq > 0 && len(r.history) > 0 && r.history[0].Seq > lastSeq+1 {
		gap = true
	}
	for _, ev := range r.history {
		if ev.Seq > lastSeq {
			sub.ch <- ev
		}
	}
	r.subs[sub] = struct{}{}
	return sub, gap
}

// unsubscribe removes a closed SSE connection.
func (r *Run) unsubscribe(sub *subscriber) {
	r.mu.Lock()
	delete(r.subs, sub)
	r.mu.Unlock()
}

// cancelRun cancels the run's context.
func (r *Run) cancelRun() {
	if r.cancel != nil {
		r.cancel()
	}
}
