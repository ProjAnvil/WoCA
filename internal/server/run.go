package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/runner"
)

// Event is one SSE frame. Seq is a per-run monotonic sequence (from 1)
// assigned by emit; it is written as the SSE id: line for Last-Event-ID
// resume and never serialized into Data.
type Event struct {
	Type string
	Data any
	Seq  int64 `json:"-"`
}

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

	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	history []Event
	pending *interruptState
	done    bool // terminal event (done or error) emitted
	seq     int64        // under mu; last assigned Seq
	dropped atomic.Int64 // fanout drops on full subscriber channels
	// onFinish, when set, fires exactly once when the terminal event is
	// emitted — on error and cancellation too, before onDone runs.
	onFinish func()
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
		final, err := runner.New(r.a, r.handleInterrupt, r.render).Run(r.ctx, history)
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
		final, err := runner.New(r.a, r.handleInterrupt, r.render).RunGoal(r.ctx, history)
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

// render translates one graph step into SSE events.
func (r *Run) render(step graph.Step) {
	if s, ok := step.Delta.(string); ok && s != "" {
		r.emit(Event{Type: "delta", Data: map[string]any{"text": s}})
		return
	}
	if rd, ok := step.Delta.(agent.ReasoningDelta); ok && rd.Text != "" {
		r.emit(Event{Type: "reasoning", Data: map[string]any{"text": rd.Text}})
		return
	}
	msgs := agent.MessagesFrom(step.Update)
	if g, ok := step.Delta.(agent.GoalDelta); ok {
		// Messages produced inside a goal execute cycle stream through the
		// delta channel; render them exactly like normal-run messages.
		msgs = append(msgs, g.Message)
	}
	for _, m := range msgs {
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

// park records the pending interrupt, emits it to clients, and blocks until an
// answer is posted or the run is cancelled.
func (r *Run) park(kind string, payload any) (any, error) {
	reply := make(chan any, 1)
	r.mu.Lock()
	r.pending = &interruptState{kind: kind, payload: payload, reply: reply}
	r.mu.Unlock()

	r.emit(Event{Type: "interrupt", Data: map[string]any{"kind": kind, "payload": payload}})

	select {
	case answer := <-reply:
		return answer, nil
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
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
			r.dropped.Add(1) // 慢消费者丢事件可观测（/api/stats）
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
