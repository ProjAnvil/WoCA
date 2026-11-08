// Package activity is the per-session activity seam (arch-cleanup spec F1):
// the snapshot/action/observer surface over a session's background work —
// todos, background shells, background subagent tasks, scheduled prompts and
// the pending-notification count — extracted from internal/agent so the seam
// logic lives without the agent god package.
//
// Dependency inversion: this package defines narrow exported interfaces for
// the manager surfaces it aggregates (Todos, Shells, Tasks, Crons, Queue,
// see seam.go); the agent package implements them with its existing
// managers. This package must never import internal/agent — the cycle is
// impossible by construction, and import_test.go guards even the test files.
// Its only woca dependency is internal/tools, which already owns the shell
// and todo snapshot shapes.
package activity

import (
	"errors"
	"time"

	"github.com/projanvil/woca/internal/tools"
)

// ActivityKind names which slice of a session's activity changed. One
// vocabulary is shared by the agent seam and the surfaces: the server's
// activity_changed payload carries these values verbatim. KindWake is
// declared here only to keep the vocabulary in one place — the server's
// wake driver emits it; the agent never does.
type ActivityKind string

const (
	KindTodos  ActivityKind = "todos"
	KindShells ActivityKind = "shells"
	KindTasks  ActivityKind = "tasks"
	KindCrons  ActivityKind = "crons"
	KindWake   ActivityKind = "wake"
)

// ErrNotFound is the seam's single not-found error: the id is unknown, or it
// exists but belongs to another session — the two are deliberately
// indistinguishable (PM §4.1). Surfaces map it to 404. (The agent package
// re-exports it as agent.ErrActivityNotFound.)
var ErrNotFound = errors.New("agent: no such background item in this session")

// ErrTaskNotFound covers both unknown ids and tasks owned by another
// session; the two cases are deliberately indistinguishable. The seam maps
// it to ErrNotFound; the task manager (the Tasks implementation) returns it.
var ErrTaskNotFound = errors.New("no such background task")

// Tail-byte bounds for the shell/task output endpoints (PM §4.3): an unset
// request gets the default; nothing above the "view all" cap is ever handed
// out, so a snapshot-sized response can never be forced through the seam.
const (
	TailBytesDefault = 4096
	TailBytesMax     = 262144
)

// clampTailBytes applies the tail bounds: maxBytes <= 0 selects the default,
// values above the cap are clamped to it.
func clampTailBytes(maxBytes int) int {
	switch {
	case maxBytes <= 0:
		return TailBytesDefault
	case maxBytes > TailBytesMax:
		return TailBytesMax
	default:
		return maxBytes
	}
}

// TaskStatus is the lifecycle state of a background task.
type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskStopped   TaskStatus = "stopped"
)

// BackgroundTaskInfo is a snapshot of one background task: identity, honest
// status, stop attribution and timestamps. EndedAt is zero while running;
// StoppedBy is "" unless the task was stopped. Mirrors
// tools.BackgroundShellStatus so the activity seam aggregates both shapes
// uniformly.
type BackgroundTaskInfo struct {
	ID          string
	Session     string
	Description string
	Subagent    string
	Status      TaskStatus
	StoppedBy   tools.StopOrigin
	StartedAt   time.Time
	EndedAt     time.Time // zero while running
}

// CronInfo is a snapshot of one scheduled entry: identity plus the three
// timestamps. LastFiredAt is zero when the entry never fired; NextFireAt is
// zero when no matching minute exists ahead (e.g. "0 0 31 2 *": February
// 31st never exists).
type CronInfo struct {
	ID, Session, Name, Spec, Prompt    string
	CreatedAt, LastFiredAt, NextFireAt time.Time
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

// ReleaseReport is what Release tore down for the session.
type ReleaseReport struct {
	ShellsKilled int
	TasksStopped int
	CronsDeleted int
	TodosDropped bool
}

// Observer is the single change callback for the whole activity seam: it
// fires with the affected session id and which slice changed whenever any
// of the four managers (todos, shells, tasks, crons) changes. It runs
// synchronously on the changing goroutine, outside every manager lock; it
// must not block — hand off to a channel and return. Released sessions
// never fire it.
type Observer func(session string, kind ActivityKind)
