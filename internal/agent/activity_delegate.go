package agent

import (
	"github.com/projanvil/woca/internal/agent/activity"
	"github.com/projanvil/woca/internal/tools"
)

// The activity seam lives in internal/agent/activity (arch-cleanup spec F1,
// dependency inversion): that subpackage defines the snapshot/action/
// observer surface plus narrow interfaces for the managers it aggregates;
// this package implements them with its existing managers and owns exactly
// one Seam instance (Agent.activity, wired in New). This file is the public
// delegation surface: the Agent methods internal/server and the sdk call
// keep their exact signatures (now thin one-line delegations), and the
// seam's exported identifiers keep their historical agent-package names via
// aliases and re-exports below, so no caller's imports change.

// The managers satisfy the seam's interfaces directly — the assertions pin
// the wiring (a method renamed away from its interface shape breaks here).
var (
	_ activity.Todos  = (*tools.TodoStore)(nil)
	_ activity.Shells = (*tools.BackgroundShells)(nil)
	_ activity.Tasks  = (*TaskManager)(nil)
	_ activity.Crons  = (*cronScheduler)(nil)
	_ activity.Queue  = (*queue)(nil)
)

// Re-exported seam vocabulary, historical names preserved.

type (
	// ActivityKind names which slice of a session's activity changed; the
	// server's activity_changed payload carries the values verbatim.
	ActivityKind = activity.ActivityKind
	// Activity is the per-session activity snapshot (todos, shells, tasks,
	// crons, pending notifications) in one shape for the panel.
	Activity = activity.Activity
	// ReleaseReport is what ReleaseSession tore down for the session.
	ReleaseReport = activity.ReleaseReport
	// TaskStatus is the lifecycle state of a background task.
	TaskStatus = activity.TaskStatus
	// BackgroundTaskInfo is a snapshot of one background task.
	BackgroundTaskInfo = activity.BackgroundTaskInfo
	// CronInfo is a snapshot of one scheduled entry.
	CronInfo = activity.CronInfo
)

const (
	ActivityTodos  = activity.KindTodos
	ActivityShells = activity.KindShells
	ActivityTasks  = activity.KindTasks
	ActivityCrons  = activity.KindCrons
	// ActivityWake is emitted by the server's wake driver only.
	ActivityWake = activity.KindWake
)

const (
	// ActivityTailBytesDefault is the tail bound an unset request gets.
	ActivityTailBytesDefault = activity.TailBytesDefault
	// ActivityTailBytesMax caps any tail request (the "view all" bound).
	ActivityTailBytesMax = activity.TailBytesMax
)

const (
	TaskRunning   = activity.TaskRunning
	TaskCompleted = activity.TaskCompleted
	TaskFailed    = activity.TaskFailed
	TaskStopped   = activity.TaskStopped
)

var (
	// ErrActivityNotFound is the seam's single not-found error: unknown and
	// foreign ids are deliberately indistinguishable. Surfaces map it to 404.
	ErrActivityNotFound = activity.ErrNotFound
	// ErrTaskNotFound is the task manager's not-found error, mapped by the
	// seam to ErrActivityNotFound.
	ErrTaskNotFound = activity.ErrTaskNotFound
)

// ActivityFor snapshots everything a run in session can see, as of now.
// The unscoped session "" keeps the TUI/REPL semantics — everything; the
// server never passes it. An unknown session yields the empty snapshot,
// non-nil slices included.
func (a *Agent) ActivityFor(session string) Activity {
	return a.activity.Snapshot(session)
}

// ShellTailFor returns the last tail bytes of a shell's output plus the
// truncated flag and the full byte count, when session may see the shell.
// The request is bounded by the seam (default when unset, capped above).
// Unknown and foreign ids both return ErrActivityNotFound.
func (a *Agent) ShellTailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, st tools.BackgroundShellStatus, err error) {
	return a.activity.ShellTail(session, id, maxBytes)
}

// KillShellFor terminates a shell on behalf of by (tools.StopByUser from the
// panel, tools.StopByModel from KillShell), recording the stop attribution
// before signalling. Killing an already-finished shell changes nothing: it
// reports alreadyFinished=true with the final status (the kill-race rule,
// PM §4.6). Unknown and foreign ids return ErrActivityNotFound.
func (a *Agent) KillShellFor(session, id string, by tools.StopOrigin) (tools.BackgroundShellStatus, bool, error) {
	return a.activity.KillShell(session, id, by)
}

// TaskTailFor returns the last tail bytes of a task's buffered output
// (streamed lines while running, the final report after) plus the truncated
// flag and the full byte count, when session may see the task. Unknown and
// foreign ids both return ErrActivityNotFound.
func (a *Agent) TaskTailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, info BackgroundTaskInfo, err error) {
	return a.activity.TaskTail(session, id, maxBytes)
}

// StopTaskFor stops a running task on behalf of by (tools.StopByUser from
// the panel, tools.StopByModel from TaskStop). Stopping an already-finished
// task changes nothing: it reports alreadyFinished=true with the unchanged
// snapshot. Unknown and foreign ids return ErrActivityNotFound.
func (a *Agent) StopTaskFor(session, id string, by tools.StopOrigin) (BackgroundTaskInfo, bool, error) {
	return a.activity.StopTask(session, id, by)
}

// DeleteCronFor removes a scheduled prompt when session may touch it. An
// entry that exists but belongs to another session answers exactly like an
// unknown id: ErrActivityNotFound.
func (a *Agent) DeleteCronFor(session, id string) error {
	return a.activity.DeleteCron(session, id)
}

// ReleaseSession tears a server session down (session deletion, D2): it
// kills the session's running shells, stops its running tasks (both with
// user attribution), deletes its scheduled prompts, drops its todo list and
// its queued items, removes the session's shell and task records entirely
// (F4), and marks the session released. From that mark on, the session is
// silent forever: late completions are dropped instead of waking or
// delivering, and no activity observer callback fires for it again.
//
// Only items the session owns are touched — unscoped ("") ones belong to
// the local single-session surface, not to this session. Releasing an
// unknown (or already released) session and the unscoped "" session are
// no-ops with a zero report. Process-wide teardown is Close's job, not
// this one.
func (a *Agent) ReleaseSession(session string) ReleaseReport {
	return a.activity.Release(session)
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
	a.activity.SetObserver(fn)
}
