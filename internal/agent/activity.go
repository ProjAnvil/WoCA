package agent

import (
	"errors"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/tools"
)

// ActivityKind names which slice of a session's activity changed. One
// vocabulary is shared by the agent seam and the surfaces: the server's
// activity_changed payload carries these values verbatim. ActivityWake is
// declared here only to keep the vocabulary in one place — the server's
// wake driver emits it; the agent never does.
type ActivityKind string

const (
	ActivityTodos  ActivityKind = "todos"
	ActivityShells ActivityKind = "shells"
	ActivityTasks  ActivityKind = "tasks"
	ActivityCrons  ActivityKind = "crons"
	ActivityWake   ActivityKind = "wake"
)

// ErrActivityNotFound is the seam's single not-found error: the id is
// unknown, or it exists but belongs to another session — the two are
// deliberately indistinguishable (PM §4.1). Surfaces map it to 404.
var ErrActivityNotFound = errors.New("agent: no such background item in this session")

// Tail-byte bounds for the shell/task output endpoints (PM §4.3): an unset
// request gets the default; nothing above the "view all" cap is ever handed
// out, so a snapshot-sized response can never be forced through the seam.
const (
	ActivityTailBytesDefault = 4096
	ActivityTailBytesMax     = 262144
)

// clampTailBytes applies the tail bounds: maxBytes <= 0 selects the default,
// values above the cap are clamped to it.
func clampTailBytes(maxBytes int) int {
	switch {
	case maxBytes <= 0:
		return ActivityTailBytesDefault
	case maxBytes > ActivityTailBytesMax:
		return ActivityTailBytesMax
	default:
		return maxBytes
	}
}

// Activity is the per-session activity snapshot (PM §4.3): the todo list,
// background shells, background subagent tasks, scheduled prompts and the
// pending-notification count, in one shape for the panel. It carries
// manager facts, not API wording — the server maps the facts onto the
// status vocabulary of PM §4.2:
//
//	shells: Running → "running"; done and !Killed → exit code 0 ?
//	        "completed" : "failed"; Killed → "stopped" (with stopped_by)
//	tasks:  Status already IS the vocabulary (TaskFailed included);
//	        "stopped" when Status == TaskStopped (stopped_by set)
//
// Decision carried from the Task 2 review — a natural exit racing a kill
// (Killed && ExitCode >= 0: the process finished on its own in the window
// between KillFor recording the stop and the signal landing): Killed wins,
// so the item renders "stopped" with stopped_by set, matching the
// completion notification, which attributes by Killed the same way. The
// exit code stays factual: report the recorded value when >= 0; null only
// for the running and signal-killed (-1) cases.
type Activity struct {
	// Todos is the session's live list, non-nil in every snapshot (empty
	// when unknown). TodosKnown distinguishes "never written" (false — the
	// caller may consult persistence) from "written, possibly cleared to
	// empty" (true): every slice in Activity is non-nil so the server can
	// render [] instead of null without per-field nil checks.
	Todos          []tools.Todo
	TodosUpdatedAt time.Time
	TodosKnown     bool
	// Shells, Tasks and Crons are in creation order (numeric ids); every
	// slice is non-nil so JSON renders [] instead of null.
	Shells []tools.BackgroundShellStatus
	Tasks  []BackgroundTaskInfo
	Crons  []CronInfo
	// PendingNotifications counts the session's queued tagged
	// notifications, Quiet included (the panel's wake badge numerator).
	PendingNotifications int
}

// ReleaseReport is what ReleaseSession tore down for the session.
type ReleaseReport struct {
	ShellsKilled int
	TasksStopped int
	CronsDeleted int
	TodosDropped bool
}

// activityState is the mutable half of the seam: the installed observer and
// the released-session set (D2). Guarded by its own mutex so observer swaps
// never contend with the Agent's broader lock.
type activityState struct {
	mu       sync.Mutex
	observer func(session string, kind ActivityKind)
	released map[string]bool
}

// ActivityFor snapshots everything a run in session can see, as of now
// (now only feeds the crons' next-fire computation). The unscoped session
// "" keeps the TUI/REPL semantics — everything; the server never passes it.
// Managers enforce ownership themselves (tracing.VisibleTo); this seam does
// not re-filter. An unknown session yields the empty snapshot, non-nil
// slices included.
func (a *Agent) ActivityFor(session string, now time.Time) Activity {
	todos, updatedAt, known := a.TodosFor(session)
	if todos == nil {
		todos = []tools.Todo{} // unknown or cleared reads as [], never null
	}
	return Activity{
		Todos:                todos,
		TodosUpdatedAt:       updatedAt,
		TodosKnown:           known,
		Shells:               a.registry.Background().ListFor(session),
		Tasks:                a.tasks.ListFor(session),
		Crons:                a.cron.InfoFor(session, now),
		PendingNotifications: a.QueuedNotificationCountFor(session),
	}
}

// ShellTailFor returns the last clampTailBytes(maxBytes) bytes of a shell's
// output plus the truncated flag and the full byte count, when session may
// see the shell. Unknown and foreign ids both return ErrActivityNotFound.
func (a *Agent) ShellTailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, st tools.BackgroundShellStatus, err error) {
	tail, truncated, total, st, err = a.registry.Background().TailFor(session, id, clampTailBytes(maxBytes))
	if errors.Is(err, tools.ErrShellNotFound) {
		err = ErrActivityNotFound
	}
	return tail, truncated, total, st, err
}

// KillShellFor terminates a shell on behalf of by (tools.StopByUser from the
// panel, tools.StopByModel from KillShell), recording the stop attribution
// before signalling. Killing an already-finished shell changes nothing: it
// reports alreadyFinished=true with the final status (the kill-race rule,
// PM §4.6). Unknown and foreign ids return ErrActivityNotFound.
func (a *Agent) KillShellFor(session, id string, by tools.StopOrigin) (tools.BackgroundShellStatus, bool, error) {
	st, already, err := a.registry.Background().KillFor(session, id, by)
	if errors.Is(err, tools.ErrShellNotFound) {
		err = ErrActivityNotFound
	}
	return st, already, err
}

// TaskTailFor returns the last clampTailBytes(maxBytes) bytes of a task's
// buffered output (streamed lines while running, the final report after)
// plus the truncated flag and the full byte count, when session may see the
// task. Unknown and foreign ids both return ErrActivityNotFound.
func (a *Agent) TaskTailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, info BackgroundTaskInfo, err error) {
	tail, truncated, total, info, err = a.tasks.TailFor(session, id, clampTailBytes(maxBytes))
	if errors.Is(err, ErrTaskNotFound) {
		err = ErrActivityNotFound
	}
	return tail, truncated, total, info, err
}

// StopTaskFor stops a running task on behalf of by (tools.StopByUser from
// the panel, tools.StopByModel from TaskStop). Stopping an already-finished
// task changes nothing: it reports alreadyFinished=true with the unchanged
// snapshot. Unknown and foreign ids return ErrActivityNotFound.
func (a *Agent) StopTaskFor(session, id string, by tools.StopOrigin) (BackgroundTaskInfo, bool, error) {
	info, already, err := a.tasks.StopFor(session, id, by)
	if errors.Is(err, ErrTaskNotFound) {
		err = ErrActivityNotFound
	}
	return info, already, err
}

// DeleteCronFor removes a scheduled prompt when session may touch it. An
// entry that exists but belongs to another session answers exactly like an
// unknown id: ErrActivityNotFound.
func (a *Agent) DeleteCronFor(session, id string) error {
	if !a.cron.Delete(id, session) {
		return ErrActivityNotFound
	}
	return nil
}

// ReleaseSession tears a server session down (session deletion, D2): it
// kills the session's running shells, stops its running tasks (both with
// user attribution), deletes its scheduled prompts, drops its todo list and
// its queued items, and marks the session released. From that mark on, the
// session is silent forever: late completions are dropped in
// queueNotification instead of waking or delivering, and no activity
// observer callback fires for it again.
//
// Only items the session owns are touched — unscoped ("") ones belong to
// the local single-session surface, not to this session. Termination is
// asynchronous, like the kill endpoint: the signals are sent, the processes
// settle on their own. Releasing an unknown (or already released) session
// and the unscoped "" session are no-ops with a zero report. Process-wide
// teardown is Close's job, not this one.
func (a *Agent) ReleaseSession(session string) ReleaseReport {
	var rep ReleaseReport
	if session == "" {
		return rep
	}
	// Mark first: everything below (and every late completion racing it)
	// is silenced for this session from here on.
	a.activity.mu.Lock()
	if a.activity.released == nil {
		a.activity.released = map[string]bool{}
	}
	a.activity.released[session] = true
	a.activity.mu.Unlock()

	// Queued items scoped to the session die with it; unscoped items and
	// other sessions' items stay queued.
	a.inputs.dropScope(session)

	// Running shells: kill with user attribution. The completions queue
	// Quiet (D1) and are dropped above regardless — a deleted session is
	// never woken.
	for _, st := range a.registry.Background().ListFor(session) {
		if st.Session != session || !st.Running {
			continue
		}
		if _, already, err := a.registry.Background().KillFor(session, st.ID, tools.StopByUser); err == nil && !already {
			rep.ShellsKilled++
		}
	}
	// Running tasks: stop with user attribution, same quiet semantics.
	for _, info := range a.tasks.ListFor(session) {
		if info.Session != session || info.Status != TaskRunning {
			continue
		}
		if _, already, err := a.tasks.StopFor(session, info.ID, tools.StopByUser); err == nil && !already {
			rep.TasksStopped++
		}
	}
	// Scheduled prompts owned by the session are deleted outright: a
	// deleted session must never be woken by its own schedule.
	for _, e := range a.cron.List(session) {
		if e.Session != session {
			continue
		}
		if a.cron.Delete(e.ID, session) {
			rep.CronsDeleted++
		}
	}
	// The todo bucket goes with the session.
	if _, _, ok := a.TodosFor(session); ok {
		a.registry.TodoStore().Drop(session)
		rep.TodosDropped = true
	}
	return rep
}

// SetActivityObserver installs fn as the single change callback for the
// whole activity seam: it fires with the affected session id and which
// slice changed whenever any of the four managers (todos, shells, tasks,
// crons) changes — each manager's hook is wired once, at Agent
// construction. fn runs synchronously on the changing goroutine (tool call,
// shell wait, task run or scheduler tick), outside every manager lock; it
// must not block — hand off to a channel and return. nil uninstalls.
// Released sessions never fire it.
func (a *Agent) SetActivityObserver(fn func(session string, kind ActivityKind)) {
	a.activity.mu.Lock()
	defer a.activity.mu.Unlock()
	a.activity.observer = fn
}

// emitActivity fans one manager change out to the installed observer. It is
// the single destination of the four manager hooks; the kind is fixed by
// whichever manager wired it. Called outside every manager lock (each
// manager fires its hook after releasing its own); the observer too is
// invoked with no lock held.
func (a *Agent) emitActivity(session string, kind ActivityKind) {
	a.activity.mu.Lock()
	fn := a.activity.observer
	silenced := session != "" && a.activity.released[session]
	a.activity.mu.Unlock()
	if fn == nil || silenced {
		return
	}
	fn(session, kind)
}

// sessionReleased reports whether session was released (ReleaseSession) and
// must no longer receive notifications. Safe to call while holding the
// queue lock: it only takes activity.mu, and nothing anywhere holds
// activity.mu and then acquires the queue lock — ReleaseSession marks under
// activity.mu, releases it, and only then drops under the queue lock — so
// q.mu → activity.mu is the one nesting order and no cycle exists.
func (a *Agent) sessionReleased(session string) bool {
	a.activity.mu.Lock()
	defer a.activity.mu.Unlock()
	return a.activity.released[session]
}
