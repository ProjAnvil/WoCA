package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent/queue"
	"github.com/projanvil/woca/internal/tools"
)

// The mid-turn queue's buffer mechanics live in internal/agent/queue
// (queue-extract spec, same recipe as the activity extraction): that
// subpackage owns QueuedInput, the thread-safe Buffer (Add/AddTagged/Drain/
// Count/WakeableCount/TaggedCount/DropScope), the tagged rendering
// (Format/Coalesce) and the run-scope read (SessionFrom) — and imports
// nothing from this package. This file is the policy and delegation surface:
// the Agent keeps every public signature unchanged (thin one-line
// delegations to its Buffer) and owns exactly the decisions the mechanics
// must not make — the released-session predicate (handed to AddTagged so the
// decision stays inside the buffer's critical section, R1), the Quiet
// decision (queueBackgroundCompletion), the notification observer firing
// (outside every lock, only after an item actually queued), and the
// run-start prepend composition (prependQueued). QueuedInput keeps its
// historical agent-package name via the alias below, so no caller's imports
// or types change.

// QueuedInput is one unit of mid-turn input; aliased at the queue
// subpackage since the extraction. See queue.QueuedInput for the field
// semantics.
type QueuedInput = queue.QueuedInput

// CoalesceQueuedInputs renders several queued inputs as ONE Human message:
// each item's tagged block (exactly what queue.Format produces) joined by a
// blank line. Surfaces use it to fold everything that accumulated while the
// agent was busy into a single message. A nil/empty slice yields an empty
// Human message; callers with nothing queued skip the call. Thin delegate
// to queue.Coalesce since the extraction.
func CoalesceQueuedInputs(items []QueuedInput) messages.Message {
	return queue.Coalesce(items)
}

// sessionFromCtx returns the session id the ctx's run is scoped to; a thin
// delegate to the queue subpackage's SessionFrom (cron, task and the run
// paths below share it). "" means unscoped delivery (the whole buffer).
func sessionFromCtx(ctx context.Context) string {
	return queue.SessionFrom(ctx)
}

// QueueInput buffers user input submitted while a run is in flight. The
// next completed tool round attaches it after its results (see toolsNode);
// input queued after the final tool round is delivered at the start of the
// next run (see prependQueued). Safe for concurrent use.
func (a *Agent) QueueInput(text string) {
	a.inputs.Add(text)
}

// QueueInputFor is the session-scoped QueueInput: the input is delivered to
// a run in that session only — its next tool round, or the start of its
// next run — and never to another session's runs. The web server's steer
// endpoint (POST /api/runs/{id}/input) queues through it (the run id names
// the session), so a steer typed for one session's in-flight run can
// neither ride another session's tool-round drain nor be discarded by
// another session's release/wake paths, which drop by exact session scope.
// An empty session keeps QueueInput's unscoped semantics (TUI/REPL). A
// released session's steer is refused like any other queued item (D2).
// Steers do not fire the notification observer: the user typed the input
// for a run they can see, not to wake a session.
func (a *Agent) QueueInputFor(session, text string) {
	a.inputs.AddTagged(a.released, QueuedInput{Text: text, Session: session})
}

// released is the queue's released-session predicate (D2), answering from
// the activity seam's Released set. A zero-value Agent (constructed outside
// New — tests only) has no seam and answers false, the same empty-set
// answer a zero-value seam state gave before the extraction (F1).
func (a *Agent) released(session string) bool {
	if a.activity == nil {
		return false
	}
	return a.activity.Released(session)
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

// QueueQuietNotificationFor is the Quiet QueueNotificationFor (D1): the item
// is delivered with the next run in that session but never starts one — the
// wake drivers ignore it. User-initiated stops queue here so the agent does
// not restart over what the user just stopped.
func (a *Agent) QueueQuietNotificationFor(session, tag, text string) {
	a.queueNotification(QueuedInput{Text: text, Tag: tag, Session: session, Quiet: true})
}

// queueBackgroundCompletion routes one background shell/task completion into
// the session-scoped notification queue. A user-initiated stop queues Quiet:
// the notification is attributed ("... by the user") and delivered with the
// next run, but never wakes the session (D1). Every other origin — natural
// completion, failure, a model KillShell/TaskStop — keeps the wake
// behaviour.
func (a *Agent) queueBackgroundCompletion(session, text string, stoppedBy tools.StopOrigin) {
	const tag = "background-task-completed"
	if stoppedBy == tools.StopByUser {
		a.QueueQuietNotificationFor(session, tag, strings.TrimSpace(text))
		return
	}
	a.QueueNotificationFor(session, tag, strings.TrimSpace(text))
}

// queueNotification queues one notification item and fires the observer
// (if installed) after the item is queued, Quiet items included (with Quiet
// set, so surfaces can show them without waking). Blank text queues nothing
// and fires nothing — the observer contract is "the item you just saw is in
// the queue". Notifications scoped to a released session (ReleaseSession,
// D2) are refused inside AddTagged's critical section — atomic with
// the release's mark-then-DropScope — so a completion racing the release
// can never leave a queued item, a wake, or an observer fire for a session
// that was deleted.
func (a *Agent) queueNotification(q QueuedInput) {
	if !a.inputs.AddTagged(a.released, q) {
		if strings.TrimSpace(q.Text) != "" && q.Session != "" {
			slog.Debug("agent: dropped notification for released session",
				"session", q.Session, "tag", q.Tag)
		}
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
// notification is queued (QueueNotification/QueueNotificationFor/
// QueueQuietNotificationFor — Quiet items arrive with Quiet set and must not
// start runs). fn runs synchronously on the producer's goroutine —
// background shells, tasks and cron completions — so it must not block;
// hand off to a channel and return. nil uninstalls the observer. This is
// the seam UI surfaces (web server, TUI, REPL) use to surface completions
// without polling the queue.
func (a *Agent) SetNotificationObserver(fn func(QueuedInput)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notifyObserver = fn
}

// DrainQueuedInputs returns and clears the queued mid-turn inputs.
func (a *Agent) DrainQueuedInputs() []QueuedInput {
	return a.inputs.Drain("")
}

// DrainQueuedInputsFor returns and clears the queued inputs a run in session
// would receive: that session's own items plus the unscoped ones.
func (a *Agent) DrainQueuedInputsFor(session string) []QueuedInput {
	return a.inputs.Drain(session)
}

// QueuedInputCount reports how many inputs are currently queued.
func (a *Agent) QueuedInputCount() int {
	return a.inputs.Count("")
}

// QueuedInputCountFor reports how many inputs a run in session would drain.
func (a *Agent) QueuedInputCountFor(session string) int {
	return a.inputs.Count(session)
}

// WakeableCountFor reports how many wake-worthy inputs a run in session
// would drain: everything except Quiet items. Wake drivers gate on this, not
// QueuedInputCountFor, so a Quiet notification never starts a run (D1).
func (a *Agent) WakeableCountFor(session string) int {
	return a.inputs.WakeableCount(session)
}

// DropScopedInputsFor removes every queued item scoped exactly to session
// (steers and notifications alike) and reports how many went. Unlike
// DrainQueuedInputsFor it has no visibility semantics: unscoped items —
// deliverable by any run — and other sessions' items are never touched.
// The wake driver's tombstone branch discards a deleted session's queue
// through it, so discarding session A can never eat an item queued for
// session B.
func (a *Agent) DropScopedInputsFor(session string) int {
	return a.inputs.DropScope(session)
}

// QueuedNotificationCountFor reports how many tagged notification items
// (Quiet included) a run in session would drain — the pending-notification
// count surfaces show.
func (a *Agent) QueuedNotificationCountFor(session string) int {
	return a.inputs.TaggedCount(session)
}

// prependQueued delivers run-start queued input: it drains the items scoped
// to ctx's session and inserts them just before history's trailing Human
// message, so a notification that landed while the agent was idle is seen at
// the start of the next run instead of waiting for a tool round that may
// never come. History not ending in Human (fork/compaction edge shapes)
// appends at the end; an empty queue returns history as-is.
func (a *Agent) prependQueued(ctx context.Context, history []messages.Message) []messages.Message {
	items := a.inputs.Drain(sessionFromCtx(ctx))
	if len(items) == 0 {
		return history
	}
	rendered := make([]messages.Message, len(items))
	for i, q := range items {
		rendered[i] = queue.Format(q)
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
