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

// interruptState holds a parked graph interrupt awaiting an HTTP answer.
type interruptState struct {
	kind    string
	payload any
	reply   chan any
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
	seq        int64        // under mu; last assigned Seq
	dropped    atomic.Int64 // fanout drops on full subscriber channels
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

// start runs the agent loop in a goroutine. onDone fires exactly once on
// every exit path; final is nil when the run ended with an error or cancel
// (nothing to persist), which lets callers hook post-run cleanup (session
// gate release) to a single point.
func (r *Run) start(history []messages.Message, onDone func([]messages.Message)) {
	go func() {
		final, err := runner.New(r.a, r.handleInterrupt, r.render).WithThreadID(r.thread).Run(r.ctx, history)
		if err != nil {
			if r.ctx.Err() != nil {
				r.finish("done", map[string]any{"cancelled": true})
			} else {
				r.finish("error", map[string]any{"message": err.Error()})
			}
			if onDone != nil {
				onDone(nil)
			}
			return
		}
		r.finish("done", map[string]any{"cancelled": false})
		if onDone != nil {
			onDone(final)
		}
	}()
}

// startGoal launches the goal loop (assess → execute → evaluate cycles) as a
// run, streaming its progress through the same SSE event vocabulary plus
// "goal" events pushed by the server on every goal-state change. onDone
// fires exactly once on every exit path; final is nil on error or cancel.
func (r *Run) startGoal(history []messages.Message, onDone func([]messages.Message)) {
	go func() {
		final, err := runner.New(r.a, r.handleInterrupt, r.render).WithThreadID(r.thread).RunGoal(r.ctx, history)
		if err != nil {
			if r.ctx.Err() != nil {
				r.finish("done", map[string]any{"cancelled": true})
			} else {
				r.finish("error", map[string]any{"message": err.Error()})
			}
			if onDone != nil {
				onDone(nil)
			}
			return
		}
		r.finish("done", map[string]any{"cancelled": false})
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
				r.emit(Event{Type: "delta", Data: map[string]any{"text": v}})
			}
		case agent.ReasoningDelta:
			if v.Text != "" {
				r.emit(Event{Type: "reasoning", Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalNote:
			// Goal-loop narration (banners, per-cycle chatter) streams as its
			// own event type so the chat never mistakes it for the answer.
			if v.Text != "" {
				r.emit(Event{Type: "goal_note", Data: map[string]any{"text": v.Text}})
			}
		case agent.GoalFinalReply:
			// The goal run's single unified reply — the one goal text that
			// reaches the chat as an answer.
			if v.Text != "" {
				r.emit(Event{Type: "delta", Data: map[string]any{"text": v.Text}})
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
			r.emit(Event{Type: "tool_call", Data: map[string]any{
				"id":   tc.ID,
				"name": tc.Name,
				"args": tc.Args,
			}})
		}
	case messages.RoleTool:
		r.emit(Event{Type: "tool_result", Data: map[string]any{
			"tool_call_id": m.ToolCallID,
			"content":      m.Content,
		}})
	}
}

// handleInterrupt classifies a graph interrupt and parks it awaiting an answer.
func (r *Run) handleInterrupt(value any) (any, error) {
	switch v := value.(type) {
	case agent.PermissionRequest:
		return r.park("permission", v)
	case agent.AskUserRequest:
		return r.park("ask", v)
	case agent.PlanApprovalRequest:
		return r.park("plan", v)
	default:
		return nil, fmt.Errorf("server: unknown interrupt value %T", value)
	}
}

// parkedRunTTL bounds how long a run may sit parked on an unanswered
// interrupt before it is cancelled (local decision — no industry
// precedent; see spec §6.3).
var parkedRunTTL = 2 * time.Hour

// park records the pending interrupt, emits it to clients, and blocks until an
// answer is posted or the run is cancelled. Unanswered interrupts are also
// bounded by parkedRunTTL: the run emits a timeout event and is cancelled so a
// forgotten ask cannot pin a session forever.
func (r *Run) park(kind string, payload any) (any, error) {
	reply := make(chan any, 1)
	r.mu.Lock()
	r.pending = &interruptState{kind: kind, payload: payload, reply: reply}
	r.mu.Unlock()

	r.emit(Event{Type: "interrupt", Data: map[string]any{"kind": kind, "payload": payload}})
	if r.onPark != nil {
		r.onPark()
	}

	timer := time.AfterFunc(parkedRunTTL, func() {
		r.emit(Event{Type: "timeout", Data: map[string]any{"kind": kind, "reason": "parked_ttl"}})
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

// answer decodes an HTTP answer and resumes the parked interrupt.
func (r *Run) answer(body []byte) error {
	r.mu.Lock()
	p := r.pending
	r.mu.Unlock()
	if p == nil {
		return errors.New("no pending interrupt")
	}

	var resume any
	switch p.kind {
	case "ask":
		var req struct {
			Answers []string `json:"answers"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("decode answer: %w", err)
		}
		resume = agent.AskUserAnswer{Answers: req.Answers}
	default: // permission, plan
		var req struct {
			Approve bool `json:"approve"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("decode answer: %w", err)
		}
		resume = req.Approve
	}

	r.mu.Lock()
	if r.pending == p {
		r.pending = nil
	}
	r.mu.Unlock()

	// The resolution is part of the event history: a client replaying the
	// stream (page refresh, /api/events-driven reattach) must not resurrect
	// the interrupt card this answer just satisfied.
	r.emit(Event{Type: "interrupt_resolved", Data: map[string]any{"kind": p.kind}})
	if r.onResume != nil {
		r.onResume()
	}

	select {
	case p.reply <- resume:
	default:
	}
	return nil
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
}

func (r *Run) droppedCount() int64 { return r.dropped.Load() }

// finish emits the terminal event (done or error) and marks the run finished
// so the server can prune it from its registry once it ages out.
func (r *Run) finish(evType string, data map[string]any) {
	r.mu.Lock()
	first := !r.done
	r.done = true
	r.finishKind = evType
	hook := r.onFinish
	r.mu.Unlock()
	r.emit(Event{Type: evType, Data: data})
	if first && hook != nil {
		hook()
	}
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
