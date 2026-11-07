package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tracing"
)

// Mid-turn input queue: input submitted while a run is in flight is buffered
// and, once the current tool round completes, appended AFTER the tool
// results so the model can address it within the same turn — the API forbids
// interleaving tool results with user messages, so the attachment must come
// after the whole result batch. Each Agent owns its queue, so subagents
// (separate Agent instances) never receive the parent's queued input — the
// drain scope is per-agent. Items also carry a session scope: the web server
// runs many sessions through one Agent, and a notification queued for one
// session must not leak into another's run. Session "" is unscoped and is
// delivered to any run (the TUI/REPL single-session case).

// QueuedInput is one unit of mid-turn input. A non-empty Tag wraps the text
// in a differently-named element (e.g. background-task completions) so the
// model can tell machine notifications apart from typed user input. A
// non-empty Session scopes delivery to runs in that session.
type QueuedInput struct {
	Text    string
	Tag     string
	Session string
}

// queuedInputTag wraps queued text so the model can tell it apart from the
// original prompt.
const queuedInputTag = "queued-user-input"

// formatQueuedInput renders one queued input as a message.
func formatQueuedInput(q QueuedInput) messages.Message {
	tag := q.Tag
	if tag == "" {
		tag = queuedInputTag
	}
	return messages.Human("<" + tag + ">\n" + strings.TrimSpace(q.Text) + "\n</" + tag + ">")
}

// CoalesceQueuedInputs renders several queued inputs as ONE Human message:
// each item's tagged block (exactly what formatQueuedInput produces) joined
// by a blank line. Surfaces use it to fold everything that accumulated
// while the agent was busy into a single message. A nil/empty slice yields
// an empty Human message; callers with nothing queued skip the call.
func CoalesceQueuedInputs(items []QueuedInput) messages.Message {
	blocks := make([]string, len(items))
	for i, q := range items {
		blocks[i] = formatQueuedInput(q).Content
	}
	return messages.Human(strings.Join(blocks, "\n\n"))
}

// sessionFromCtx returns the session id the ctx's run is scoped to. TUI/REPL
// runs carry no run scope; "" means unscoped delivery (the whole queue).
func sessionFromCtx(ctx context.Context) string {
	if sc, ok := tracing.RunFrom(ctx); ok {
		return sc.SessionID
	}
	return ""
}

// queue is the thread-safe mid-turn input buffer.
type queue struct {
	mu    sync.Mutex
	items []QueuedInput
}

func (q *queue) add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, QueuedInput{Text: text})
}

// addTagged buffers a tagged notification (machine-generated input) scoped
// to session; "" is unscoped. It reports whether an item was queued — blank
// text is dropped, nothing queues.
func (q *queue) addTagged(session, tag, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, QueuedInput{Text: text, Tag: tag, Session: session})
	return true
}

// drain removes and returns the items a run in session receives: everything
// when unscoped ("" — the single-session callers see the whole queue),
// otherwise the unscoped items plus the session's own. Items scoped to other
// sessions stay queued, in arrival order.
func (q *queue) drain(session string) []QueuedInput {
	q.mu.Lock()
	defer q.mu.Unlock()
	if session == "" {
		items := q.items
		q.items = nil
		return items
	}
	var out []QueuedInput
	keep := q.items[:0]
	for _, it := range q.items {
		if tracing.VisibleTo(it.Session, session) {
			out = append(out, it)
		} else {
			keep = append(keep, it)
		}
	}
	q.items = keep
	return out
}

// count reports how many items a run in session would drain (len for the
// unscoped "" case).
func (q *queue) count(session string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if session == "" {
		return len(q.items)
	}
	n := 0
	for _, it := range q.items {
		if tracing.VisibleTo(it.Session, session) {
			n++
		}
	}
	return n
}

// QueueInput buffers user input submitted while a run is in flight. The
// next completed tool round attaches it after its results (see toolsNode);
// input queued after the final tool round is delivered at the start of the
// next run (see prependQueued). Safe for concurrent use.
func (a *Agent) QueueInput(text string) {
	a.inputs.add(text)
}

// QueueNotification buffers a machine-generated notification (e.g. a
// background-shell completion) for delivery after the next tool round,
// wrapped in its own tag so the model can tell it from typed user input.
func (a *Agent) QueueNotification(tag, text string) {
	a.queueNotification(QueuedInput{Text: text, Tag: tag})
}

// QueueNotificationFor is the session-scoped QueueNotification: the
// notification is delivered to the next run in that session (or to any
// unscoped run) and never to other sessions' runs.
func (a *Agent) QueueNotificationFor(session, tag, text string) {
	a.queueNotification(QueuedInput{Text: text, Tag: tag, Session: session})
}

// queueNotification queues one notification item and fires the observer
// (if installed) after the item is queued. Blank text queues nothing and
// fires nothing — the observer contract is "the item you just saw is in the
// queue".
func (a *Agent) queueNotification(q QueuedInput) {
	if !a.inputs.addTagged(q.Session, q.Tag, q.Text) {
		return
	}
	a.mu.Lock()
	fn := a.notifyObserver
	a.mu.Unlock()
	if fn != nil {
		fn(q)
	}
}

// SetNotificationObserver installs fn as the callback fired whenever a
// notification is queued (QueueNotification/QueueNotificationFor). fn runs
// synchronously on the producer's goroutine — background shells, tasks and
// cron completions — so it must not block; hand off to a channel and return.
// nil uninstalls the observer. This is the seam UI surfaces (web server,
// TUI, REPL) use to surface completions without polling the queue.
func (a *Agent) SetNotificationObserver(fn func(QueuedInput)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notifyObserver = fn
}

// DrainQueuedInputs returns and clears the queued mid-turn inputs.
func (a *Agent) DrainQueuedInputs() []QueuedInput {
	return a.inputs.drain("")
}

// DrainQueuedInputsFor returns and clears the queued inputs a run in session
// would receive: that session's own items plus the unscoped ones.
func (a *Agent) DrainQueuedInputsFor(session string) []QueuedInput {
	return a.inputs.drain(session)
}

// QueuedInputCount reports how many inputs are currently queued.
func (a *Agent) QueuedInputCount() int {
	return a.inputs.count("")
}

// QueuedInputCountFor reports how many inputs a run in session would drain.
func (a *Agent) QueuedInputCountFor(session string) int {
	return a.inputs.count(session)
}

// prependQueued delivers run-start queued input: it drains the items scoped
// to ctx's session and inserts them just before history's trailing Human
// message, so a notification that landed while the agent was idle is seen at
// the start of the next run instead of waiting for a tool round that may
// never come. History not ending in Human (fork/compaction edge shapes)
// appends at the end; an empty queue returns history as-is.
func (a *Agent) prependQueued(ctx context.Context, history []messages.Message) []messages.Message {
	items := a.inputs.drain(sessionFromCtx(ctx))
	if len(items) == 0 {
		return history
	}
	rendered := make([]messages.Message, len(items))
	for i, q := range items {
		rendered[i] = formatQueuedInput(q)
	}
	at := len(history)
	if at > 0 && history[at-1].Role == messages.RoleHuman {
		at-- // ahead of the fresh user turn, not after it
	}
	out := make([]messages.Message, 0, len(history)+len(items))
	out = append(out, history[:at]...)
	out = append(out, rendered...)
	out = append(out, history[at:]...)
	return out
}
