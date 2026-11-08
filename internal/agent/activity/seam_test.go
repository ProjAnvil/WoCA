package activity

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// The seam tests pin subpackage policy against the REAL tools managers
// (todos, shells — both leaf-package types the seam's interfaces were cut
// from) and small fakes for the three manager surfaces that live inside
// internal/agent (tasks, crons, queue) and therefore cannot be constructed
// here. The manager-side behavior itself (visibility rules, kill-race
// recording, queue atomicity) stays pinned by the integration tests in
// internal/agent/activity_test.go, which drive the same seam through the
// real Agent wiring.

// fakeTasks is a minimal Tasks implementation: a static list plus recorded
// actions.
type fakeTasks struct {
	list    []BackgroundTaskInfo // returned verbatim by ListFor
	stopped []string             // ids passed to StopFor
	dropped []string             // sessions passed to DropSession
}

func (f *fakeTasks) ListFor(session string) []BackgroundTaskInfo {
	out := make([]BackgroundTaskInfo, 0, len(f.list))
	for _, info := range f.list {
		if info.Session == session || info.Session == "" {
			out = append(out, info)
		}
	}
	return out
}

func (f *fakeTasks) TailFor(session, id string, maxBytes int) (string, bool, int, BackgroundTaskInfo, error) {
	for _, info := range f.list {
		if info.ID != id {
			continue
		}
		if info.Session != session && session != "" {
			return "", false, 0, BackgroundTaskInfo{}, ErrTaskNotFound
		}
		return fmt.Sprintf("tail of %s", id), false, 100, info, nil
	}
	return "", false, 0, BackgroundTaskInfo{}, ErrTaskNotFound
}

func (f *fakeTasks) StopFor(session, id string, by tools.StopOrigin) (BackgroundTaskInfo, bool, error) {
	for i, info := range f.list {
		if info.ID != id {
			continue
		}
		if info.Session != session && session != "" {
			return BackgroundTaskInfo{}, false, ErrTaskNotFound
		}
		if info.Status != TaskRunning {
			return info, true, nil
		}
		f.stopped = append(f.stopped, id)
		info.Status = TaskStopped
		info.StoppedBy = by
		f.list[i] = info
		return info, false, nil
	}
	return BackgroundTaskInfo{}, false, ErrTaskNotFound
}

func (f *fakeTasks) DropSession(session string) int {
	f.dropped = append(f.dropped, session)
	keep := f.list[:0]
	dropped := 0
	for _, info := range f.list {
		if info.Session != session {
			keep = append(keep, info)
		} else {
			dropped++
		}
	}
	f.list = keep
	return dropped
}

// fakeCrons is a minimal Crons implementation over owned and unscoped
// entries.
type fakeCrons struct {
	entries []CronInfo
	deleted []string
}

func (f *fakeCrons) InfoFor(session string) []CronInfo {
	out := make([]CronInfo, 0, len(f.entries))
	for _, e := range f.entries {
		if e.Session == session || e.Session == "" {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeCrons) Delete(id, session string) bool {
	for i, e := range f.entries {
		if e.ID != id {
			continue
		}
		if e.Session != session && session != "" {
			return false // exists, but another session owns it
		}
		f.deleted = append(f.deleted, id)
		f.entries = append(f.entries[:i], f.entries[i+1:]...)
		return true
	}
	return false
}

// fakeQueue is a minimal Queue implementation: per-session tagged counts
// plus recorded scoped drops.
type fakeQueue struct {
	counts  map[string]int
	dropped []string
}

func (f *fakeQueue) TaggedCount(session string) int { return f.counts[session] }

func (f *fakeQueue) DropScope(session string) int {
	f.dropped = append(f.dropped, session)
	n := f.counts[session]
	delete(f.counts, session)
	return n
}

// recorder is a concurrency-safe observer sink (mirrors the agent-level
// helper; exit callbacks fire from wait goroutines even here).
type recorder struct {
	mu     sync.Mutex
	events []activityEvent
}

type activityEvent struct {
	session string
	kind    ActivityKind
}

func (r *recorder) observe(session string, kind ActivityKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, activityEvent{session, kind})
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// newSeam wires a Seam over the real tools managers (todos, shells) and the
// given fakes.
func newSeam(t *testing.T, tasks *fakeTasks, crons *fakeCrons, q *fakeQueue) (*Seam, *tools.TodoStore, *tools.BackgroundShells) {
	t.Helper()
	todos := tools.NewTodoStore()
	shells := tools.NewBackgroundShells()
	if tasks == nil {
		tasks = &fakeTasks{}
	}
	if crons == nil {
		crons = &fakeCrons{}
	}
	if q == nil {
		q = &fakeQueue{counts: map[string]int{}}
	}
	return New(Deps{Todos: todos, Shells: shells, Tasks: tasks, Crons: crons, Queue: q}), todos, shells
}

func TestSnapshotAggregatesAndNormalizes(t *testing.T) {
	s, todos, _ := newSeam(t, &fakeTasks{list: []BackgroundTaskInfo{
		{ID: "task-1", Session: "sess-1", Status: TaskRunning},
	}}, &fakeCrons{entries: []CronInfo{{ID: "cron-1", Session: "sess-1", Name: "nightly"}}},
		&fakeQueue{counts: map[string]int{"sess-1": 2}})
	todos.Set("sess-1", []tools.Todo{{Content: "write tests", Status: "pending"}})

	snap := s.Snapshot("sess-1")
	if !snap.TodosKnown || len(snap.Todos) != 1 || snap.Todos[0].Content != "write tests" {
		t.Fatalf("todos = %+v (known=%v)", snap.Todos, snap.TodosKnown)
	}
	if snap.TodosUpdatedAt.IsZero() {
		t.Fatal("todos updated-at is zero")
	}
	if len(snap.Shells) != 0 || snap.Shells == nil {
		t.Fatalf("shells = %+v", snap.Shells)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != "task-1" || snap.Tasks[0].Status != TaskRunning {
		t.Fatalf("tasks = %+v", snap.Tasks)
	}
	if len(snap.Crons) != 1 || snap.Crons[0].Name != "nightly" {
		t.Fatalf("crons = %+v", snap.Crons)
	}
	if snap.PendingNotifications != 2 {
		t.Fatalf("pending notifications = %d, want 2", snap.PendingNotifications)
	}

	// An unknown session yields the empty snapshot, every slice non-nil
	// (Todos normalized by the seam, the rest non-nil from their managers),
	// TodosKnown alone carrying "never written".
	empty := s.Snapshot("no-such-session")
	if empty.Todos == nil || empty.Shells == nil || empty.Tasks == nil || empty.Crons == nil {
		t.Fatalf("unknown-session snapshot has nil slices: %+v", empty)
	}
	if len(empty.Todos) != 0 || len(empty.Shells) != 0 || len(empty.Tasks) != 0 ||
		len(empty.Crons) != 0 || empty.PendingNotifications != 0 || empty.TodosKnown {
		t.Fatalf("unknown-session snapshot not empty: %+v", empty)
	}

	// A known-but-cleared todo list renders as an empty, non-nil slice.
	todos.Set("sess-empty", nil)
	known := s.Snapshot("sess-empty")
	if !known.TodosKnown || known.Todos == nil || len(known.Todos) != 0 {
		t.Fatalf("cleared todos = %+v (known=%v)", known.Todos, known.TodosKnown)
	}
}

func TestShellTailClampsAndMapsNotFound(t *testing.T) {
	s, _, shells := newSeam(t, nil, nil, nil)
	ctx := tracing.WithRun(t.Context(), "run-t", "sess-t")

	id, err := shells.Start(ctx, "echo 0123456789", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shells.WaitFor(t.Context(), "sess-t", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	tail, truncated, total, st, err := s.ShellTail("sess-t", id, 4)
	if err != nil || !truncated || total != 11 || tail != "789\n" || st.ExitCode != 0 {
		t.Fatalf("ShellTail = %q truncated=%v total=%d st=%+v err=%v", tail, truncated, total, st, err)
	}
	// An unset (<=0) request gets the default bound, not the whole buffer.
	if _, truncated, total, _, err := s.ShellTail("sess-t", id, 0); err != nil || truncated || total != 11 {
		t.Fatalf("default-bound tail: truncated=%v total=%d err=%v", truncated, total, err)
	}
	// Requests above the cap are clamped to it.
	if _, _, _, _, err := s.ShellTail("sess-t", id, 1<<20); err != nil {
		t.Fatalf("clamped tail: %v", err)
	}
	if _, _, _, _, err := s.ShellTail("sess-foreign", id, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign ShellTail err = %v, want ErrNotFound", err)
	}
	if _, _, _, _, err := s.ShellTail("sess-t", "999", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ShellTail err = %v, want ErrNotFound", err)
	}
}

func TestKillShellPassesThroughRaceAndMapsNotFound(t *testing.T) {
	s, _, shells := newSeam(t, nil, nil, nil)
	ctx := tracing.WithRun(t.Context(), "run-k", "sess-k")

	// Finished shell: killing it reports already-finished, status unchanged
	// (the kill-race rule, PM §4.6).
	id, err := shells.Start(ctx, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shells.WaitFor(t.Context(), "sess-k", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	st, already, err := s.KillShell("sess-k", id, tools.StopByUser)
	if err != nil || !already || st.Running || st.Killed {
		t.Fatalf("KillShell finished = %+v already=%v err=%v", st, already, err)
	}
	if _, _, err := s.KillShell("sess-foreign", id, tools.StopByUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign KillShell err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.KillShell("sess-k", "999", tools.StopByUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown KillShell err = %v, want ErrNotFound", err)
	}
}

func TestTaskTailAndStopMapNotFound(t *testing.T) {
	tasks := &fakeTasks{list: []BackgroundTaskInfo{
		{ID: "task-1", Session: "sess-1", Status: TaskRunning},
	}}
	s, _, _ := newSeam(t, tasks, nil, nil)

	tail, _, _, info, err := s.TaskTail("sess-1", "task-1", 6)
	if err != nil || info.ID != "task-1" || tail != "tail of task-1" {
		t.Fatalf("TaskTail = %q info=%+v err=%v", tail, info, err)
	}
	if _, _, _, _, err := s.TaskTail("sess-foreign", "task-1", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign TaskTail err = %v, want ErrNotFound", err)
	}
	if _, _, _, _, err := s.TaskTail("sess-1", "task-999", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown TaskTail err = %v, want ErrNotFound", err)
	}

	got, already, err := s.StopTask("sess-1", "task-1", tools.StopByUser)
	if err != nil || already || got.Status != TaskStopped || got.StoppedBy != tools.StopByUser {
		t.Fatalf("StopTask = %+v already=%v err=%v", got, already, err)
	}
	if got, already, err = s.StopTask("sess-1", "task-1", tools.StopByUser); err != nil || !already || got.Status != TaskStopped {
		t.Fatalf("StopTask again = %+v already=%v err=%v", got, already, err)
	}
	if _, _, err := s.StopTask("sess-foreign", "task-1", tools.StopByUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign StopTask err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.StopTask("sess-1", "task-999", tools.StopByUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown StopTask err = %v, want ErrNotFound", err)
	}
}

func TestDeleteCronMapsNotFound(t *testing.T) {
	s, _, _ := newSeam(t, nil, &fakeCrons{entries: []CronInfo{
		{ID: "cron-1", Session: "sess-1"},
		{ID: "cron-u", Session: ""},
	}}, nil)

	if err := s.DeleteCron("sess-1", "cron-1"); err != nil {
		t.Fatalf("DeleteCron: %v", err)
	}
	if err := s.DeleteCron("sess-1", "cron-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete err = %v, want ErrNotFound", err)
	}
	// An entry another session owns answers exactly like an unknown id.
	if err := s.DeleteCron("sess-2", "cron-u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign-owned err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCron("sess-1", "cron-999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v, want ErrNotFound", err)
	}
}

func TestReleaseOrchestratesAndSilences(t *testing.T) {
	tasks := &fakeTasks{list: []BackgroundTaskInfo{
		{ID: "task-live", Session: "sess-rel", Status: TaskRunning},
		{ID: "task-done", Session: "sess-rel", Status: TaskCompleted},
		{ID: "task-other", Session: "sess-other", Status: TaskRunning},
	}}
	crons := &fakeCrons{entries: []CronInfo{
		{ID: "cron-rel", Session: "sess-rel"},
		{ID: "cron-unscoped", Session: ""},
	}}
	q := &fakeQueue{counts: map[string]int{"sess-rel": 3, "sess-other": 1}}
	s, todos, shells := newSeam(t, tasks, crons, q)
	rec := &recorder{}
	s.SetObserver(rec.observe)

	rel := tracing.WithRun(t.Context(), "run-rel", "sess-rel")
	liveID, err := shells.Start(rel, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	doneID, err := shells.Start(rel, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shells.WaitFor(t.Context(), "sess-rel", doneID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	otherID, err := shells.Start(tracing.WithRun(t.Context(), "run-o", "sess-other"), "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = shells.KillFor("sess-other", otherID, tools.StopByModel) })
	todos.Set("sess-rel", []tools.Todo{{Content: "doomed", Status: "pending"}})

	rep := s.Release("sess-rel")
	if rep.ShellsKilled != 1 || rep.TasksStopped != 1 || rep.CronsDeleted != 1 || !rep.TodosDropped {
		t.Fatalf("release report = %+v", rep)
	}

	// The queue was asked to drop the session's scoped items; the manager
	// records went with it entirely (F4) — the finished shell's too.
	if len(q.dropped) != 1 || q.dropped[0] != "sess-rel" {
		t.Fatalf("queue drops = %v", q.dropped)
	}
	if _, _, err := shells.OutputFor("sess-rel", liveID); !errors.Is(err, tools.ErrShellNotFound) {
		t.Fatalf("released shell record still resolvable: %v", err)
	}
	if _, _, err := shells.OutputFor("sess-rel", doneID); !errors.Is(err, tools.ErrShellNotFound) {
		t.Fatalf("finished shell record survived release: %v", err)
	}
	if st, err := shells.StatusFor("sess-other", otherID); err != nil || !st.Running {
		t.Fatalf("foreign shell disturbed by release: err=%v st=%+v", err, st)
	}

	// Only the owned running task was stopped, with user attribution; the
	// session's task records were dropped entirely, foreign ones untouched.
	if len(tasks.stopped) != 1 || tasks.stopped[0] != "task-live" {
		t.Fatalf("stopped tasks = %v", tasks.stopped)
	}
	for _, id := range tasks.stopped {
		for _, info := range tasks.list {
			if info.ID == id && info.StoppedBy != tools.StopByUser {
				t.Fatalf("task %s stopped by %q, want user", id, info.StoppedBy)
			}
		}
	}
	if len(tasks.dropped) != 1 || tasks.dropped[0] != "sess-rel" {
		t.Fatalf("task DropSession = %v", tasks.dropped)
	}
	if len(tasks.ListFor("sess-other")) != 1 {
		t.Fatalf("foreign task record disturbed: %+v", tasks.list)
	}

	// Only the owned cron was deleted; the unscoped entry stays.
	if len(crons.deleted) != 1 || crons.deleted[0] != "cron-rel" {
		t.Fatalf("deleted crons = %v", crons.deleted)
	}
	if got := len(crons.InfoFor("sess-rel")); got != 1 { // the unscoped one
		t.Fatalf("crons after release = %+v", crons.entries)
	}
	if _, _, ok := todos.Get("sess-rel"); ok {
		t.Fatal("todo bucket survived release")
	}

	// The release mark silences the observer and answers Released; other
	// sessions and the unscoped "" are unaffected.
	if !s.Released("sess-rel") {
		t.Fatal("Released(sess-rel) = false after release")
	}
	if s.Released("sess-other") {
		t.Fatal("Released(sess-other) = true without a release")
	}
	before := rec.total()
	s.Emit("sess-rel", KindTodos)
	if got := rec.total(); got != before {
		t.Fatalf("observer fired %d time(s) for a released session", got-before)
	}
	s.Emit("sess-other", KindTodos)
	if got := rec.total(); got != before+1 {
		t.Fatalf("observer fired %d time(s) for another session, want 1", got-before)
	}
	s.Emit("", KindShells)
	if got := rec.total(); got != before+2 {
		t.Fatalf("observer fired %d time(s) for the unscoped session, want 1", got-(before+1))
	}

	// Releasing again, an unknown session and the unscoped session are all
	// no-ops with a zero report.
	if rep := s.Release("sess-rel"); rep != (ReleaseReport{}) {
		t.Fatalf("re-release report = %+v, want zero", rep)
	}
	if rep := s.Release("never-existed"); rep != (ReleaseReport{}) {
		t.Fatalf("unknown-session release report = %+v, want zero", rep)
	}
	if rep := s.Release(""); rep != (ReleaseReport{}) {
		t.Fatalf("unscoped release report = %+v, want zero", rep)
	}
}

func TestObserverSwapAndUninstall(t *testing.T) {
	s, _, _ := newSeam(t, nil, nil, nil)
	rec := &recorder{}
	s.SetObserver(rec.observe)
	s.Emit("sess-o", KindTasks)
	if rec.total() != 1 {
		t.Fatalf("events = %d, want 1", rec.total())
	}
	// nil uninstalls: further changes fire nothing.
	s.SetObserver(nil)
	s.Emit("sess-o", KindTasks)
	if rec.total() != 1 {
		t.Fatalf("events after uninstall = %d, want 1", rec.total())
	}
}
