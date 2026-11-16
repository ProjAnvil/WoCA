package activity

import (
	"errors"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/tools"
)

// The interfaces below are the seam's entire view of its managers: each one
// carries exactly the methods the seam calls (the Go-proverb cut — e.g.
// Snapshot reads ListFor/InfoFor/Get/TaggedCount, Release adds the Drop*
// and Delete actions), never the managers' full surface. The agent package
// satisfies them with its existing managers:
//
//	Todos  ← *tools.TodoStore        Shells ← *tools.BackgroundShells
//	Tasks  ← *agent.TaskManager      Crons  ← *agent.cronScheduler
//	Queue  ← *queue.Buffer
//
// (asserted in internal/agent/activity_delegate.go).

// Todos is the todo surface: the snapshot read and the per-session drop.
type Todos interface {
	// Get returns the session's todo list, its last-update time, and
	// whether the session ever wrote one (ok=false distinguishes "never
	// written" from "written, possibly cleared to empty").
	Get(session string) (todos []tools.Todo, updatedAt time.Time, ok bool)
	// Drop discards the session's todo list entirely.
	Drop(session string)
}

// Shells is the background-shell surface: list/tail/kill per session plus
// the per-session record drop.
type Shells interface {
	ListFor(session string) []tools.BackgroundShellStatus
	TailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, st tools.BackgroundShellStatus, err error)
	KillFor(session, id string, by tools.StopOrigin) (st tools.BackgroundShellStatus, alreadyFinished bool, err error)
	DropSession(session string) int
}

// Tasks is the background-subagent-task surface: list/tail/stop per session
// plus the per-session record drop. Unknown and foreign ids answer
// ErrTaskNotFound. Both Drop returns report how many records went; the seam
// ignores them.
type Tasks interface {
	ListFor(session string) []BackgroundTaskInfo
	TailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, info BackgroundTaskInfo, err error)
	StopFor(session, id string, by tools.StopOrigin) (info BackgroundTaskInfo, alreadyFinished bool, err error)
	DropSession(session string) int
}

// Crons is the scheduled-prompt surface: the snapshot read (used both by
// Snapshot and by Release's delete-own-entries loop — same visibility rule,
// same order) and the scoped delete. Delete answers false for unknown ids
// and entries another session owns, exactly like an unknown id.
type Crons interface {
	InfoFor(session string) []CronInfo
	Delete(id, session string) bool
}

// Queue is the notification-queue surface: the pending-notification count
// surfaces report (tagged items, Quiet included) and the scoped drop that
// kills a released session's queued items.
type Queue interface {
	TaggedCount(session string) int
	DropScope(session string) int
}

// Subagents is the foreground-subagent-children surface (graph-native HITL
// §3.7): the session-filtered snapshot read feeding the Subagents tab. The
// registry behind it has no seam-side actions (children are stopped through
// their parent run, not the panel), so Snapshot is its entire seam use.
type Subagents interface {
	ListFor(session string) []SubagentRunInfo
}

// Deps bundles the manager surfaces the seam aggregates. Every field must
// be non-nil; the agent package is the only production constructor and
// always wires all six.
type Deps struct {
	Todos     Todos
	Shells    Shells
	Tasks     Tasks
	Crons     Crons
	Queue     Queue
	Subagents Subagents
}

// Seam is the activity seam: one instance per process, owned by the agent,
// aggregating the five manager surfaces into the snapshot/action/observer
// API the surfaces (server, TUI, REPL) consume through the agent's
// delegating methods. Its mutable half is the installed observer and the
// released-session set (D2), guarded by its own mutex so observer swaps
// never contend with the agent's broader lock.
type Seam struct {
	deps Deps

	mu       sync.Mutex
	observer Observer
	released map[string]bool
}

// New builds a Seam over deps.
func New(deps Deps) *Seam {
	return &Seam{deps: deps}
}

// Snapshot returns everything a run in session can see, as of now. The
// unscoped session "" keeps the TUI/REPL semantics — everything; the server
// never passes it. Managers enforce ownership themselves
// (tracing.VisibleTo); this seam does not re-filter. An unknown session
// yields the empty snapshot, non-nil slices included. (Cron next-fire times
// are per-entry caches since F5, so the snapshot takes no clock.)
func (s *Seam) Snapshot(session string) Activity {
	todos, updatedAt, known := s.deps.Todos.Get(session)
	if todos == nil {
		todos = []tools.Todo{} // unknown or cleared reads as [], never null
	}
	subagents := s.deps.Subagents.ListFor(session)
	if subagents == nil {
		subagents = []SubagentRunInfo{} // the registry renders [], never null
	}
	return Activity{
		Todos:                todos,
		TodosUpdatedAt:       updatedAt,
		TodosKnown:           known,
		Shells:               s.deps.Shells.ListFor(session),
		Tasks:                s.deps.Tasks.ListFor(session),
		Crons:                s.deps.Crons.InfoFor(session),
		Subagents:            subagents,
		PendingNotifications: s.deps.Queue.TaggedCount(session),
	}
}

// ShellTail returns the last clampTailBytes(maxBytes) bytes of a shell's
// output plus the truncated flag and the full byte count, when session may
// see the shell. Unknown and foreign ids both return ErrNotFound.
func (s *Seam) ShellTail(session, id string, maxBytes int) (tail string, truncated bool, total int, st tools.BackgroundShellStatus, err error) {
	tail, truncated, total, st, err = s.deps.Shells.TailFor(session, id, clampTailBytes(maxBytes))
	if errors.Is(err, tools.ErrShellNotFound) {
		err = ErrNotFound
	}
	return tail, truncated, total, st, err
}

// KillShell terminates a shell on behalf of by (tools.StopByUser from the
// panel, tools.StopByModel from KillShell), recording the stop attribution
// before signalling. Killing an already-finished shell changes nothing: it
// reports alreadyFinished=true with the final status (the kill-race rule,
// PM §4.6). Unknown and foreign ids return ErrNotFound.
func (s *Seam) KillShell(session, id string, by tools.StopOrigin) (tools.BackgroundShellStatus, bool, error) {
	st, already, err := s.deps.Shells.KillFor(session, id, by)
	if errors.Is(err, tools.ErrShellNotFound) {
		err = ErrNotFound
	}
	return st, already, err
}

// TaskTail returns the last clampTailBytes(maxBytes) bytes of a task's
// buffered output (streamed lines while running, the final report after)
// plus the truncated flag and the full byte count, when session may see the
// task. Unknown and foreign ids both return ErrNotFound.
func (s *Seam) TaskTail(session, id string, maxBytes int) (tail string, truncated bool, total int, info BackgroundTaskInfo, err error) {
	tail, truncated, total, info, err = s.deps.Tasks.TailFor(session, id, clampTailBytes(maxBytes))
	if errors.Is(err, ErrTaskNotFound) {
		err = ErrNotFound
	}
	return tail, truncated, total, info, err
}

// StopTask stops a running task on behalf of by (tools.StopByUser from the
// panel, tools.StopByModel from TaskStop). Stopping an already-finished
// task changes nothing: it reports alreadyFinished=true with the unchanged
// snapshot. Unknown and foreign ids return ErrNotFound.
func (s *Seam) StopTask(session, id string, by tools.StopOrigin) (BackgroundTaskInfo, bool, error) {
	info, already, err := s.deps.Tasks.StopFor(session, id, by)
	if errors.Is(err, ErrTaskNotFound) {
		err = ErrNotFound
	}
	return info, already, err
}

// DeleteCron removes a scheduled prompt when session may touch it. An
// entry that exists but belongs to another session answers exactly like an
// unknown id: ErrNotFound.
func (s *Seam) DeleteCron(session, id string) error {
	if !s.deps.Crons.Delete(id, session) {
		return ErrNotFound
	}
	return nil
}

// Release tears a server session down (session deletion, D2): it kills the
// session's running shells, stops its running tasks (both with user
// attribution), deletes its scheduled prompts, drops its todo list and its
// queued items, removes the session's shell and task records entirely
// (F4), and marks the session released. From that mark on, the session is
// silent forever: late completions are dropped at the queue (the agent
// wires Queue.DropScope and Buffer.AddTagged's reject predicate to Released)
// and no activity observer callback fires for it again.
//
// Only items the session owns are touched — unscoped ("") ones belong to
// the local single-session surface, not to this session. Termination is
// asynchronous, like the kill endpoint: the signals are sent, the
// processes settle on their own (invisibly — their records are already
// gone). Releasing an unknown (or already released) session and the
// unscoped "" session are no-ops with a zero report. Process-wide teardown
// is the agent's Close, not this one.
func (s *Seam) Release(session string) ReleaseReport {
	var rep ReleaseReport
	if session == "" {
		return rep
	}
	// Mark first: everything below (and every late completion racing it)
	// is silenced for this session from here on.
	s.mu.Lock()
	if s.released == nil {
		s.released = map[string]bool{}
	}
	s.released[session] = true
	s.mu.Unlock()

	// Queued items scoped to the session die with it; unscoped items and
	// other sessions' items stay queued.
	s.deps.Queue.DropScope(session)

	// Running shells: kill with user attribution. The completions queue
	// Quiet (D1) and are dropped above regardless — a deleted session is
	// never woken.
	for _, st := range s.deps.Shells.ListFor(session) {
		if st.Session != session || !st.Running {
			continue
		}
		if _, already, err := s.deps.Shells.KillFor(session, st.ID, tools.StopByUser); err == nil && !already {
			rep.ShellsKilled++
		}
	}
	// Running tasks: stop with user attribution, same quiet semantics.
	for _, info := range s.deps.Tasks.ListFor(session) {
		if info.Session != session || info.Status != TaskRunning {
			continue
		}
		if _, already, err := s.deps.Tasks.StopFor(session, info.ID, tools.StopByUser); err == nil && !already {
			rep.TasksStopped++
		}
	}
	// Scheduled prompts owned by the session are deleted outright: a
	// deleted session must never be woken by its own schedule. InfoFor
	// lists with the same visibility rule and order the delete loop needs;
	// only owned entries (Session == session) are removed.
	for _, info := range s.deps.Crons.InfoFor(session) {
		if info.Session != session {
			continue
		}
		if s.deps.Crons.Delete(info.ID, session) {
			rep.CronsDeleted++
		}
	}
	// The todo bucket goes with the session.
	if _, _, ok := s.deps.Todos.Get(session); ok {
		s.deps.Todos.Drop(session)
		rep.TodosDropped = true
	}
	// The session's records go with it entirely (F4): the shells killed and
	// tasks stopped above are unobservable afterwards — late completions were
	// already silenced — so keeping their records would be pure retention
	// cost. The released mark itself stays (R2).
	s.deps.Shells.DropSession(session)
	s.deps.Tasks.DropSession(session)
	return rep
}

// SetObserver installs fn as the single change callback for the whole
// activity seam: it fires with the affected session id and which slice
// changed whenever any of the four managers (todos, shells, tasks, crons)
// changes — each manager's hook is wired once, by the agent at
// construction (Emit is the destination). nil uninstalls. Released
// sessions never fire it.
func (s *Seam) SetObserver(fn Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = fn
}

// Emit fans one manager change out to the installed observer. It is the
// single destination of the four manager hooks (wired by the agent); the
// kind is fixed by whichever manager wired it. Called outside every
// manager lock (each manager fires its hook after releasing its own); the
// observer too is invoked with no lock held.
func (s *Seam) Emit(session string, kind ActivityKind) {
	s.mu.Lock()
	fn := s.observer
	silenced := session != "" && s.released[session]
	s.mu.Unlock()
	if fn == nil || silenced {
		return
	}
	fn(session, kind)
}

// Released reports whether session was released (Release) and must no
// longer receive notifications. Safe to call while holding the queue lock:
// it only takes the seam's mutex, and nothing anywhere holds that mutex
// and then acquires the queue lock — Release marks under it, releases it,
// and only then drops under the queue lock — so queue.mu → seam.mu is the
// one nesting order and no cycle exists.
func (s *Seam) Released(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released[session]
}
