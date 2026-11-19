package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// Partial-transcript accumulator for cancelled runs (spec
	// 2026-11-19-cancel-partial-output §D1). render writes on the run
	// goroutine; the cancel path reads after runFn returned. committed
	// carries complete updates-batch messages; provisional carries
	// goal-cycle messages seen via GoalDelta custom chunks that their
	// owning node's batch has not yet superseded; deltaBuf accumulates
	// streamed assistant text until an AI-bearing batch lands the complete
	// message.
	committed     []messages.Message
	provisional   []messages.Message
	committedIDs  map[string]bool
	provisionalID map[string]bool
	deltaBuf      string
	// startedAt stamps startWith entry (post-promotion — queue wait
	// excluded, parked waits included): the origin of the aborted-after-Xs
	// elapsed copy.
	startedAt time.Time
	// lastCards holds the most recent park's cards by answer key — the
	// pause the run is currently leaving (or parked on). The buffering path
	// reads it to stamp each buffered entry with its approval's identity
	// (see cardIdentity), so the entry still addresses that approval when
	// the re-park mints a fresh interrupt NS.
	lastCards map[string]parkedCard
	// buffered holds answers that arrived while no pause was pending —
	// the resume window between a partial answer and the interrupted
	// sibling's re-park (the child must re-execute its approval node
	// first). They apply to the next park whose cards they address — by
	// key, or by approval identity when the re-park regenerated the NS —
	// and entries for approvals that never re-park are dropped with that
	// park (never orphaned while live, never immortal once gone).
	buffered []cardAnswerEntry
	done     bool // terminal event (done or error) emitted
	// finishKind records the terminal event type for the run ledger.
	finishKind string
	// finishCancelled, under mu, records whether the terminal done event was
	// a cancellation. The wake driver skips cancelled (and errored) runs
	// when deciding whether to re-check the session's queue — see ranClean.
	finishCancelled bool
	// stopReason, under mu, is the stamped stop cause (see the StopReason*
	// set): written EXACTLY ONCE at the single write point (cancelRun) —
	// the first cancel wins the label and later ones cannot relabel (a
	// parked-TTL timer firing after a user stop keeps user_stop). Empty
	// while the run has not stopped; readers fall back to user_stop for
	// cancellations that predate the typed causes.
	stopReason string
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
	r.mu.Lock()
	r.startedAt = time.Now()
	r.mu.Unlock()
	go func() {
		final, err := runFn()
		if err != nil {
			if r.ctx.Err() != nil {
				reason := r.stopCause()
				// Spec 2026-11-19-cancel-partial-output §D4: every dangling
				// call of the partial transcript gets its synthesized
				// terminal result on the event stream BEFORE the done frame —
				// live tabs, reattach replay and the ledger mirror all see
				// the open tool cards close. Runs without a render face
				// (command runs) accumulate nothing, so they emit nothing.
				for _, m := range synthesizeAborted(r.partialTranscript(), reason, r.runElapsed()) {
					r.emit(Event{Type: EventToolResult, Data: toolResultPayload{
						ToolCallID: m.ToolCallID,
						Content:    m.Content,
					}})
				}
				// The done payload carries the stop cause next to
				// cancelled:true (the live UI distinguishes stop reasons
				// without waiting for the reload's persisted tag).
				r.finish(EventDone, map[string]any{"cancelled": true, "reason": reason})
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
// message batches nodes produced. The same chunks feed the partial-transcript
// accumulator (see the Run field comment) so a cancelled run can persist what
// already streamed.
func (r *Run) render(chunk lggraph.StreamChunk) {
	switch chunk.Mode {
	case lggraph.StreamCustom:
		switch v := chunk.Payload.(type) {
		case string:
			if v != "" {
				r.accumulateDelta(v)
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
				r.accumulateDelta(v.Text)
				r.emit(Event{Type: EventDelta, Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalDelta:
			// Messages produced inside a goal execute cycle stream through
			// the custom channel; render them like normal-run messages.
			r.recordProvisional(v.Message)
			r.renderMessage(v.Message)
		}
	case lggraph.StreamUpdates:
		updates, _ := chunk.Payload.(map[string]any)
		for _, nodeUpdate := range updates {
			state, _ := nodeUpdate.(map[string]any)
			msgs := agent.MessagesFrom(state)
			r.recordCommitted(msgs)
			for _, m := range msgs {
				r.renderMessage(m)
			}
		}
	}
}

// accumulateDelta appends streamed assistant text (string and GoalFinalReply
// custom chunks) to the pending buffer. The buffer is superseded — not
// completed — by the owning node's updates batch (recordCommitted): a
// cancel landing mid-stream keeps the partial text as a trailing AI message.
func (r *Run) accumulateDelta(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltaBuf += text
}

// recordProvisional remembers one goal-cycle message seen via a GoalDelta
// custom chunk. The goal graph re-delivers every cycle message in the
// execute node's updates batch when the cycle commits (dual channel), so
// provisional copies live only in the uncommitted window. Messages carrying
// a graph-stamped id are deduped per id: an inner-graph resume replays the
// completed siblings' writes (with their original ids) as GoalDelta chunks
// again, and the uncommitted window must keep each copy once.
func (r *Run) recordProvisional(m messages.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.ID != "" {
		if r.provisionalID == nil {
			r.provisionalID = map[string]bool{}
		}
		if r.provisionalID[m.ID] {
			return
		}
		r.provisionalID[m.ID] = true
	}
	r.provisional = append(r.provisional, m)
}

// recordCommitted folds one updates-batch message group into the partial
// transcript. The batch is the authoritative copy of the node's messages,
// so it discards any provisional goal-cycle copies it supersedes, and an
// AI-bearing batch flushes the streamed-text buffer (the complete message
// replaces its stream). Messages carrying a graph-stamped id are deduped
// per id: when a parked superstep resumes, the fork re-streams the
// completed siblings' writes with their ORIGINAL ids (graph.go:1446-1450)
// — writes already emitted live before the pause — and a second copy in
// the partial transcript would persist duplicate tool results a strict
// provider rejects on the next round's replay.
func (r *Run) recordCommitted(msgs []messages.Message) {
	if len(msgs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range msgs {
		if m.ID != "" {
			if r.committedIDs == nil {
				r.committedIDs = map[string]bool{}
			}
			if r.committedIDs[m.ID] {
				continue
			}
			r.committedIDs[m.ID] = true
		}
		r.committed = append(r.committed, m)
	}
	r.provisional = nil
	r.provisionalID = nil
	for _, m := range msgs {
		if m.Role == messages.RoleAI {
			r.deltaBuf = ""
			break
		}
	}
}

// partialTranscript assembles the run's partial transcript for a cancel:
// the committed batches, the provisional goal-cycle messages whose batch
// never landed, and the still-buffered streamed text as a trailing AI
// message when the model node never committed. nil when nothing streamed
// (command runs).
func (r *Run) partialTranscript() []messages.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var partial []messages.Message
	partial = append(partial, r.committed...)
	partial = append(partial, r.provisional...)
	if r.deltaBuf != "" {
		partial = append(partial, messages.AI(r.deltaBuf))
	}
	return partial
}

// runElapsed reports time since startWith entry (post-promotion); 0 before
// the run started. Parked waits count (documented in the spec: the wording
// says how long the run lived, not how long it worked).
func (r *Run) runElapsed() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startedAt.IsZero() {
		return 0
	}
	return time.Since(r.startedAt)
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
	// Remember this park's cards for the NEXT resume window's buffering
	// (identity stamping in answer()).
	last := make(map[string]parkedCard, len(parked))
	for _, c := range parked {
		last[c.key()] = c
	}
	r.lastCards = last
	// Answers buffered during the resume window apply now: entries whose
	// key addresses one of THIS park's cards decode against it and become
	// the pause's resume value (the same scalar/map rules as answer());
	// entries whose key missed because the re-park minted a fresh NS
	// re-key through the approval's identity (same asks, new address);
	// entries for approvals that never re-parked die here.
	var resume any
	if len(r.buffered) > 0 {
		byKey := make(map[string]parkedCard, len(parked))
		byIdentity := make(map[string]parkedCard, len(parked))
		for _, c := range parked {
			byKey[c.key()] = c
			if id := cardIdentity(c); id != "" {
				byIdentity[id] = c
			}
		}
		answered := make(map[string]any)
		kept := r.buffered[:0:0]
		for _, e := range r.buffered {
			card, ok := byKey[e.Key]
			if !ok && e.identity != "" {
				// Same approval under a regenerated NS: re-key the entry
				// onto the card its asks address.
				card, ok = byIdentity[e.identity]
			}
			if !ok {
				continue
			}
			value, err := cardAnswerValue(card, e.Value)
			if err != nil {
				kept = append(kept, e) // undecodable against this card: keep for a later one
				continue
			}
			answered[card.key()] = value
		}
		r.buffered = kept
		switch {
		case len(answered) == 0:
			// nothing matched; fall through to a normal wait
		case len(answered) == len(parked):
			for k := range answered {
				resume = answered[k]
				break
			}
		default:
			resume = answered // partial: the unmatched cards re-pause
		}
	}
	r.mu.Unlock()
	if resume != nil {
		reply <- resume
	}
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
		// The TTL is a stop cause like any other: cancelRun is the single
		// write point that stamps it (first writer wins — a user cancel
		// that landed first keeps user_stop).
		r.emit(Event{Type: EventTimeout, Data: map[string]any{"kind": kind, "reason": StopReasonParkedTTL}})
		r.cancelRun(StopReasonParkedTTL)
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
// identity is server-internal (never on the wire): the approval identity the
// key addressed at buffer time, so the entry survives the re-park minting a
// fresh NS for the same approval (see cardIdentity).
type cardAnswerEntry struct {
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
	identity string
}

// cardIdentity returns the stable identity of a parked card's approval
// request: its asks' call ids, sorted and joined. Those ids are the model
// tool-call ids the graph's decisions channel keys its decisions by — they
// persist in the checkpointed message state, so they survive the approval
// node's re-execution where the fork's interrupt NS is minted per
// invocation. "" for ask-less legacy cards (identity matching does not
// apply; those keep key-only addressing).
func cardIdentity(c parkedCard) string {
	if len(c.asks) == 0 {
		return ""
	}
	ids := make([]string, 0, len(c.asks))
	for _, ask := range c.asks {
		if ask.CallID == "" {
			return "" // an unaddressable ask is worse than no identity
		}
		ids = append(ids, ask.CallID)
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
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
//
// A terminal run accepts no answer at all: no park can ever consume it, so
// the list form must not buffer (the entries would ride a resume window that
// no longer exists) and nothing may emit interrupt_resolved past the
// terminal event — a replaying client would resurrect a card the done/error
// frame already closed. Late answers fail like every other unmatched shape.
// The gate is enforced twice: once on entry, and again inside the critical
// section that writes the buffer / clears the park and appends the
// resolution event, so an answer interleaving with finish() cannot emit
// past the terminal event either.
func (r *Run) answer(body []byte) error {
	r.mu.Lock()
	p := r.pending
	done := r.done
	r.mu.Unlock()
	if done {
		return errors.New("run already finished")
	}
	if p == nil {
		// The resume window (see Run.buffered): list-form answers buffer
		// and apply to the next park; the legacy single-card forms cannot
		// be addressed without their card and stay an error.
		entries, isList := decodeCardAnswerList(body)
		if !isList {
			return errors.New("no pending interrupt")
		}
		// One critical section for the done re-check, the buffer writes and
		// the resolution emission: the gate above can read done=false just
		// before finish() flips it, and a check-then-act split across two
		// lock acquisitions could append interrupt_resolved past the
		// terminal event. With the emission under the same lock as the
		// re-check, finish's set-done (which precedes its terminal append)
		// orders every accepted resolution before the terminal event.
		r.mu.Lock()
		if r.done {
			r.mu.Unlock()
			return errors.New("run already finished")
		}
		for _, e := range entries {
			// Stamp the approval identity the key addresses NOW — the
			// pause is gone, but the last park's cards still name it, and
			// the re-park may mint a fresh NS for the same approval.
			if card, ok := r.lastCards[e.Key]; ok {
				e.identity = cardIdentity(card)
			}
			r.buffered = append(r.buffered, e)
		}
		ev := r.appendEvent(Event{Type: EventInterruptResolved, Data: map[string]any{"kind": "multi"}})
		r.mu.Unlock()
		// The buffered answer satisfies the pause the run is currently
		// re-parking: without the resolution event a client watching the
		// stream keeps rendering the previous park's cards until the next
		// terminal event — the contract says interrupt_resolved fires on
		// EVERY accepted answer, and a re-park (if any) re-emits its own
		// interrupt right after. The mirror runs outside r.mu like emit's.
		if r.mirror != nil {
			r.mirror(ev)
		}
		return nil
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
		grantedKeys := map[string]bool{}
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
			// A session grant covers every OTHER pending ask of the same
			// rule key: those asks were persisted by their child's hooks
			// superstep before the grant existed, so they cannot re-evaluate
			// it on resume — answering them here (allow-once semantics, the
			// grant itself already covers the future) is the only point
			// where the collapse can happen. Ask/plan cards never match a
			// grant key and stay pending.
			for _, rule := range grantedRuleKeys(card, e.Value) {
				grantedKeys[rule] = true
			}
		}
		if len(grantedKeys) > 0 {
			for _, c := range p.cards {
				if _, done := answered[c.key()]; done {
					continue
				}
				if rule := cardGrantKey(c); rule != "" && grantedKeys[rule] {
					answered[c.key()] = allowAnswerFor(c)
				}
			}
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

	// The resolution is part of the event history: a client replaying the
	// stream (page refresh, /api/events-driven reattach) must not resurrect
	// the interrupt card this answer just satisfied.
	kind := "multi"
	if len(p.cards) == 1 {
		kind = p.cards[0].kind
	}
	// Same single-critical-section rule as the buffering branch: the done
	// re-check, the pending clear and the resolution emission are atomic, so
	// an answer racing finish() either lands its resolution before the
	// terminal event or not at all.
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return errors.New("run already finished")
	}
	if r.pending == p {
		r.pending = nil
	}
	ev := r.appendEvent(Event{Type: EventInterruptResolved, Data: map[string]any{"kind": kind}})
	r.mu.Unlock()
	if r.mirror != nil {
		r.mirror(ev)
	}
	if r.onResume != nil {
		r.onResume()
	}

	select {
	case p.reply <- resume:
	default:
	}
	return nil
}

// grantedRuleKeys extracts the session-grant rule keys of one answer entry:
// the approvals decision form carries per-ask {outcome, grant}; the rule key
// itself names the ASK (the decision map is keyed by call id).
func grantedRuleKeys(card parkedCard, raw json.RawMessage) []string {
	var v struct {
		Decisions map[string]struct {
			Grant string `json:"grant"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var keys []string
	for callID, d := range v.Decisions {
		if d.Grant != "session" {
			continue
		}
		for _, ask := range card.asks {
			if ask.CallID == callID && ask.SessionRuleKey != "" {
				keys = append(keys, ask.SessionRuleKey)
			}
		}
	}
	return keys
}

// cardGrantKey returns the single session rule key a pending card's
// permission asks share ("" for ask/plan cards, non-grantable asks, or
// mixed-key cards — those stay pending for an explicit answer).
func cardGrantKey(c parkedCard) string {
	key := ""
	for _, ask := range c.asks {
		if ask.Questions != nil || ask.Plan != "" || ask.SessionRuleKey == "" {
			return ""
		}
		if key != "" && key != ask.SessionRuleKey {
			return ""
		}
		key = ask.SessionRuleKey
	}
	return key
}

// allowAnswerFor builds the allow-once resume value for a grant-covered
// card (every permission ask allowed; the session grant already covers the
// future, so no per-ask grant repeats).
func allowAnswerFor(c parkedCard) any {
	out := agent.ApprovalAnswer{Decisions: make(map[string]agent.AskDecision, len(c.asks))}
	for _, ask := range c.asks {
		out.Decisions[ask.CallID] = agent.AskDecision{Outcome: agent.OutcomeAllow}
	}
	return out
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
	ev = r.appendEvent(ev)
	r.mu.Unlock()
	if r.mirror != nil {
		r.mirror(ev)
	}
}

// appendEvent is emit's locked body — seq assignment, history append and
// subscriber fanout — returning the seq-stamped event (the mirror persists
// that copy). Callers hold r.mu; answer() uses it directly so the
// interrupt_resolved emission shares ONE critical section with the done
// re-check, which is what keeps a resolution from ordering past the
// terminal event when an answer races finish().
func (r *Run) appendEvent(ev Event) Event {
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
	return ev
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

// cancelledReason reports whether the run ended cancelled and, if so, the
// stamped cause: a getter of the field cancelRun wrote (single write
// point), falling back to user_stop when nothing stamped a cause.
// Cancelled=false covers clean finishes and errors — runs with no stop
// notice to persist. Callers read it from the completion path, after finish
// recorded the terminal state.
func (r *Run) cancelledReason() (reason string, cancelled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finishKind != EventDone || !r.finishCancelled {
		return "", false
	}
	if r.stopReason != "" {
		return r.stopReason, true
	}
	return StopReasonUser, true
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

// cancelRun cancels the run's context, drops any parked interrupt, and
// stamps WHY the run stopped. It is the single write point for the cause:
// the stamp happens exactly once under the run mutex — the first cancel
// wins the label, and a later cancel (or a no-op repeat) cannot overwrite
// it, so a parked-TTL timer and a user cancel racing in one instruction
// window cannot mislabel the run. A cancelled run will never consume an
// answer, so a late one must fail (answer's done gate — "run already
// finished") instead of returning 200 and emitting a stray resolution
// event into the replay history.
func (r *Run) cancelRun(cause string) {
	r.mu.Lock()
	r.pending = nil
	r.lastCards = nil
	r.buffered = nil
	if cause != "" && r.stopReason == "" {
		r.stopReason = cause
	}
	r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// stopCause returns the stamped stop cause, defaulting to user_stop: the
// done event payload always carries a reason (wire compatibility —
// cancellations without an explicit cause were user stops).
func (r *Run) stopCause() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopReason != "" {
		return r.stopReason
	}
	return StopReasonUser
}
