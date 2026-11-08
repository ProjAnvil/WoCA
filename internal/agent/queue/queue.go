// Package queue implements the mid-turn input buffer mechanics: the
// thread-safe buffer that holds input submitted while a run is in flight
// (or while the agent is idle), the session-scoped drain/count/drop reads,
// and the tagged rendering that turns buffered items into messages.
//
// Mid-turn input: input submitted while a run is in flight is buffered and,
// once the current tool round completes, appended AFTER the tool results so
// the model can address it within the same turn — the API forbids
// interleaving tool results with user messages, so the attachment must come
// after the whole result batch. Each agent owns its buffer, so subagents
// (separate agents) never receive the parent's queued input — the drain
// scope is per-agent. Items also carry a session scope: the web server runs
// many sessions through one agent, and a notification queued for one session
// must not leak into another's run. Session "" is unscoped and is delivered
// to any run (the TUI/REPL single-session case).
//
// This package is mechanics only. Every policy decision is the owning
// agent's (internal/agent), injected or composed at its delegation surface
// (internal/agent/queue_delegate.go): the released-session predicate is
// handed to AddTagged as the reject callback (evaluated under the buffer
// lock), the Quiet decision and the notification observer stay with the
// agent, and where drained items land in the history (run-start prepend vs
// after-tool-results attach) is the agent's composition. The package must
// not import internal/agent (guard test).
package queue

import (
	"context"
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tracing"
)

// QueuedInput is one unit of mid-turn input. A non-empty Tag wraps the text
// in a differently-named element (e.g. background-task completions) so the
// model can tell machine notifications apart from typed user input. A
// non-empty Session scopes delivery to runs in that session. Quiet marks a
// notification that must never start a wake run (D1: a user-initiated stop):
// it is delivered with the next run — at its start (the agent's run-start
// prepend) or in a busy run's tool rounds — but the wake drivers treat it as
// absent.
type QueuedInput struct {
	Text    string
	Tag     string
	Session string
	Quiet   bool
}

// queuedInputTag wraps queued text so the model can tell it apart from the
// original prompt.
const queuedInputTag = "queued-user-input"

// Format renders one queued input as a message.
func Format(q QueuedInput) messages.Message {
	tag := q.Tag
	if tag == "" {
		tag = queuedInputTag
	}
	return messages.Human("<" + tag + ">\n" + strings.TrimSpace(q.Text) + "\n</" + tag + ">")
}

// Coalesce renders several queued inputs as ONE Human message: each item's
// tagged block (exactly what Format produces) joined by a blank line.
// Surfaces use it to fold everything that accumulated while the agent was
// busy into a single message. A nil/empty slice yields an empty Human
// message; callers with nothing queued skip the call.
func Coalesce(items []QueuedInput) messages.Message {
	blocks := make([]string, len(items))
	for i, q := range items {
		blocks[i] = Format(q).Content
	}
	return messages.Human(strings.Join(blocks, "\n\n"))
}

// SessionFrom returns the session id the ctx's run is scoped to. TUI/REPL
// runs carry no run scope; "" means unscoped delivery (the whole buffer).
func SessionFrom(ctx context.Context) string {
	if sc, ok := tracing.RunFrom(ctx); ok {
		return sc.SessionID
	}
	return ""
}

// Buffer is the thread-safe mid-turn input buffer. The zero value is ready
// to use; the owning agent is the only production constructor site (its
// inputs field).
type Buffer struct {
	mu    sync.Mutex
	items []QueuedInput
}

// Add buffers one unscoped, untagged item of typed user input. Blank text
// (whitespace-only) is dropped.
func (b *Buffer) Add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, QueuedInput{Text: text})
}

// AddTagged buffers one tagged notification item (machine-generated input),
// Quiet flag included. reject, when non-nil, is the agent's released-session
// predicate (D2) and is consulted INSIDE the buffer's critical section: an
// item scoped to a session the predicate reports as released is refused —
// deciding under b.mu makes check-and-append atomic against DropScope (the
// activity seam's release path), so a completion racing the release can
// never land after the drop. It reports whether an item was queued — blank
// text is dropped and a released session is refused, nothing queues.
func (b *Buffer) AddTagged(reject func(session string) bool, item QueuedInput) bool {
	if strings.TrimSpace(item.Text) == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if item.Session != "" && reject != nil && reject(item.Session) {
		return false
	}
	b.items = append(b.items, item)
	return true
}

// Drain removes and returns the items a run in session receives: everything
// when unscoped ("" — the single-session callers see the whole buffer),
// otherwise the unscoped items plus the session's own. Items scoped to other
// sessions stay buffered, in arrival order.
func (b *Buffer) Drain(session string) []QueuedInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == "" {
		items := b.items
		b.items = nil
		return items
	}
	var out []QueuedInput
	keep := b.items[:0]
	for _, it := range b.items {
		if tracing.VisibleTo(it.Session, session) {
			out = append(out, it)
		} else {
			keep = append(keep, it)
		}
	}
	b.items = keep
	return out
}

// Count reports how many items a run in session would drain (len for the
// unscoped "" case).
func (b *Buffer) Count(session string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == "" {
		return len(b.items)
	}
	n := 0
	for _, it := range b.items {
		if tracing.VisibleTo(it.Session, session) {
			n++
		}
	}
	return n
}

// WakeableCount is Count over the non-Quiet items: what a wake driver may
// start a run for. Quiet items are delivered like any other, they just never
// trigger the run (D1).
func (b *Buffer) WakeableCount(session string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, it := range b.items {
		if !it.Quiet && tracing.VisibleTo(it.Session, session) {
			n++
		}
	}
	return n
}

// TaggedCount is Count over the tagged notification items (Quiet included):
// the pending-notification count surfaces report. (Part of the
// activity.Queue interface the buffer satisfies.)
func (b *Buffer) TaggedCount(session string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, it := range b.items {
		if it.Tag != "" && tracing.VisibleTo(it.Session, session) {
			n++
		}
	}
	return n
}

// DropScope removes every item scoped exactly to session and reports how
// many went; unscoped items and other sessions' items stay buffered, in
// order. Session release (D2): a deleted session's pending input dies with
// it instead of lingering where nothing will ever drain it. (Part of the
// activity.Queue interface the buffer satisfies.)
func (b *Buffer) DropScope(session string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := b.items[:0]
	dropped := 0
	for _, it := range b.items {
		if it.Session == session {
			dropped++
			continue
		}
		keep = append(keep, it)
	}
	b.items = keep
	return dropped
}
