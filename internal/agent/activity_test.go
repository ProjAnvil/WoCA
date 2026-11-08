package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// newActivityAgent builds a real Agent (real registry, real wiring) for the
// activity-seam tests; the fake model is never invoked because no run starts.
func newActivityAgent(t *testing.T) (*Agent, *tools.Registry) {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir())
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       reg,
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, reg
}

// callTodoWrite drives the real TodoWrite tool (the tool layer owns todo
// semantics) with ctx's run scope, so the write lands in that session's
// bucket exactly as a model call would.
func callTodoWrite(t *testing.T, reg *tools.Registry, ctx context.Context, todos ...tools.Todo) {
	t.Helper()
	var todoTool coretools.Tool
	for _, tl := range reg.All() {
		if tl.Name() == "TodoWrite" {
			todoTool = tl
			break
		}
	}
	if todoTool == nil {
		t.Fatal("TodoWrite tool not registered")
	}
	items := make([]map[string]any, len(todos))
	for i, td := range todos {
		items[i] = map[string]any{"content": td.Content, "status": td.Status}
	}
	if _, err := todoTool.Invoke(ctx, map[string]any{"todos": items}); err != nil {
		t.Fatalf("TodoWrite: %v", err)
	}
}

// activityEvent is one recorded observer callback.
type activityEvent struct {
	session string
	kind    ActivityKind
}

// activityRecorder is a concurrency-safe observer sink.
type activityRecorder struct {
	mu     sync.Mutex
	events []activityEvent
}

func (r *activityRecorder) observe(session string, kind ActivityKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, activityEvent{session, kind})
}

func (r *activityRecorder) count(session string, kind ActivityKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.session == session && e.kind == kind {
			n++
		}
	}
	return n
}

func (r *activityRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func TestActivitySnapshotAggregatesAllManagers(t *testing.T) {
	a, reg := newActivityAgent(t)
	s1 := tracing.WithRun(t.Context(), "run-1", "sess-1")

	callTodoWrite(t, reg, s1, tools.Todo{Content: "write tests", Status: "pending"})
	if _, err := reg.Background().Start(s1, "echo snapshot", "", "."); err != nil {
		t.Fatal(err)
	}
	task, err := a.tasks.start(s1, "subject", "description", "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.cron.Add("sess-1", "nightly", "run the checks", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	a.QueueNotificationFor("sess-1", "tag", "hello")

	snap := a.ActivityFor("sess-1")

	if !snap.TodosKnown || len(snap.Todos) != 1 || snap.Todos[0].Content != "write tests" {
		t.Fatalf("todos = %+v (known=%v)", snap.Todos, snap.TodosKnown)
	}
	if snap.TodosUpdatedAt.IsZero() {
		t.Fatal("todos updated-at is zero")
	}
	if len(snap.Shells) != 1 || snap.Shells[0].Session != "sess-1" || snap.Shells[0].Command != "echo snapshot" || !snap.Shells[0].Running {
		t.Fatalf("shells = %+v", snap.Shells)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != task.ID || snap.Tasks[0].Status != TaskRunning {
		t.Fatalf("tasks = %+v", snap.Tasks)
	}
	if len(snap.Crons) != 1 || snap.Crons[0].Name != "nightly" || snap.Crons[0].Prompt != "run the checks" {
		t.Fatalf("crons = %+v", snap.Crons)
	}
	if snap.Crons[0].NextFireAt.IsZero() || !snap.Crons[0].LastFiredAt.IsZero() {
		t.Fatalf("cron timestamps = %+v", snap.Crons[0])
	}
	if snap.PendingNotifications != 1 {
		t.Fatalf("pending notifications = %d, want 1", snap.PendingNotifications)
	}

	// A foreign session sees none of it (ownership lives in the managers).
	other := a.ActivityFor("sess-2")
	if other.TodosKnown || len(other.Todos) != 0 || len(other.Shells) != 0 ||
		len(other.Tasks) != 0 || len(other.Crons) != 0 || other.PendingNotifications != 0 {
		t.Fatalf("foreign snapshot = %+v", other)
	}

	// The unscoped session ("" — TUI/REPL semantics) sees everything.
	all := a.ActivityFor("")
	if len(all.Shells) != 1 || len(all.Tasks) != 1 || len(all.Crons) != 1 {
		t.Fatalf("unscoped snapshot = %+v", all)
	}

	// An unknown session yields the empty snapshot (every slice non-nil —
	// including Todos; TodosKnown alone carries the "never written" fact —
	// so the server renders 200 with empty arrays, PM §4.3).
	empty := a.ActivityFor("no-such-session")
	if empty.Shells == nil || empty.Tasks == nil || empty.Crons == nil || empty.Todos == nil {
		t.Fatalf("unknown-session snapshot has nil slices: %+v", empty)
	}
	if len(empty.Shells) != 0 || len(empty.Tasks) != 0 || len(empty.Crons) != 0 || len(empty.Todos) != 0 || empty.TodosKnown {
		t.Fatalf("unknown-session snapshot not empty: %+v", empty)
	}
	// A known-but-cleared todo list renders as an empty, non-nil slice.
	a.SetTodosFor("sess-empty", nil)
	known := a.ActivityFor("sess-empty")
	if !known.TodosKnown || known.Todos == nil || len(known.Todos) != 0 {
		t.Fatalf("cleared todos = %+v (known=%v)", known.Todos, known.TodosKnown)
	}
}

func TestActivityTailsAreBounded(t *testing.T) {
	a, reg := newActivityAgent(t)
	s1 := tracing.WithRun(t.Context(), "run-t", "sess-t")

	id, err := reg.Background().Start(s1, "echo 0123456789", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Background().WaitFor(t.Context(), "sess-t", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	tail, truncated, total, st, err := a.ShellTailFor("sess-t", id, 4)
	if err != nil || !truncated || total != 11 || tail != "789\n" || st.ExitCode != 0 {
		t.Fatalf("ShellTailFor = %q truncated=%v total=%d st=%+v err=%v", tail, truncated, total, st, err)
	}
	// An unset (<=0) request gets the default bound, not the whole buffer.
	if _, truncated, total, _, err := a.ShellTailFor("sess-t", id, 0); err != nil || truncated || total != 11 {
		t.Fatalf("default-bound tail: truncated=%v total=%d err=%v", truncated, total, err)
	}
	// Requests above the cap are clamped to it.
	if _, _, _, _, err := a.ShellTailFor("sess-t", id, 1<<20); err != nil {
		t.Fatalf("clamped tail: %v", err)
	}

	task, err := a.tasks.start(s1, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	task.appendOutput("hello task output")
	tail, truncated, total, info, err := a.TaskTailFor("sess-t", task.ID, 6)
	if err != nil || !truncated || total != 17 || tail != "output" || info.ID != task.ID {
		t.Fatalf("TaskTailFor = %q truncated=%v total=%d err=%v", tail, truncated, total, err)
	}
}

func TestClampTailBytes(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, ActivityTailBytesDefault},
		{-5, ActivityTailBytesDefault},
		{100, 100},
		{ActivityTailBytesMax, ActivityTailBytesMax},
		{ActivityTailBytesMax + 1, ActivityTailBytesMax},
		{1 << 20, ActivityTailBytesMax},
	}
	for _, tc := range cases {
		if got := clampTailBytes(tc.in); got != tc.want {
			t.Errorf("clampTailBytes(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestActivityStopEntryPoints(t *testing.T) {
	a, reg := newActivityAgent(t)
	s1 := tracing.WithRun(t.Context(), "run-s", "sess-s")

	// A finished shell: killing it reports already-finished, status unchanged
	// (the UI's kill-race rule, PM §4.6).
	id, err := reg.Background().Start(s1, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Background().WaitFor(t.Context(), "sess-s", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// The natural completion queues from the shell's wait goroutine — wait
	// for it, then drain, so later counts see only what they should.
	waitForQueue := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if a.QueuedInputCountFor("sess-s") == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("queued items = %d, want %d", a.QueuedInputCountFor("sess-s"), want)
	}
	waitForQueue(1)
	a.DrainQueuedInputsFor("sess-s") // drop the natural completion's notification
	st, already, err := a.KillShellFor("sess-s", id, tools.StopByUser)
	if err != nil || !already || st.Running || st.Killed {
		t.Fatalf("KillShellFor finished = %+v already=%v err=%v", st, already, err)
	}

	// A running shell killed by the user: killed, attributed, and the
	// completion is a Quiet notification (D1 — never wakeable).
	liveID, err := reg.Background().Start(s1, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	st, already, err = a.KillShellFor("sess-s", liveID, tools.StopByUser)
	if err != nil || already || !st.Killed || st.StoppedBy != tools.StopByUser {
		t.Fatalf("KillShellFor running = %+v already=%v err=%v", st, already, err)
	}
	final, err := reg.Background().WaitFor(t.Context(), "sess-s", liveID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final.Running {
		t.Fatal("killed shell still running")
	}
	waitForQueue(1)
	if n := a.WakeableCountFor("sess-s"); n != 0 {
		t.Fatalf("user kill left %d wakeable items, want 0 (D1)", n)
	}

	// Tasks: stop records user attribution; stopping again changes nothing.
	task, err := a.tasks.start(s1, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	info, already, err := a.StopTaskFor("sess-s", task.ID, tools.StopByUser)
	if err != nil || already || info.Status != TaskStopped || info.StoppedBy != tools.StopByUser {
		t.Fatalf("StopTaskFor = %+v already=%v err=%v", info, already, err)
	}
	info, already, err = a.StopTaskFor("sess-s", task.ID, tools.StopByUser)
	if err != nil || !already || info.Status != TaskStopped {
		t.Fatalf("StopTaskFor again = %+v already=%v err=%v", info, already, err)
	}

	// Crons: delete by the owning session.
	cronID, err := a.cron.Add("sess-s", "n", "p", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteCronFor("sess-s", cronID); err != nil {
		t.Fatalf("DeleteCronFor: %v", err)
	}
	if err := a.DeleteCronFor("sess-s", cronID); !errors.Is(err, ErrActivityNotFound) {
		t.Fatalf("re-delete err = %v, want ErrActivityNotFound", err)
	}
}

func TestActivityForeignAndUnknownIDsAreNotFound(t *testing.T) {
	a, reg := newActivityAgent(t)
	s1 := tracing.WithRun(t.Context(), "run-f", "sess-own")

	shellID, err := reg.Background().Start(s1, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.tasks.start(s1, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	cronID, err := a.cron.Add("sess-own", "n", "p", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })

	const foreign = "sess-foreign"
	if _, _, _, _, err := a.ShellTailFor(foreign, shellID, 10); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("ShellTailFor foreign: %v", err)
	}
	if _, _, err := a.KillShellFor(foreign, shellID, tools.StopByUser); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("KillShellFor foreign: %v", err)
	}
	if _, _, _, _, err := a.TaskTailFor(foreign, task.ID, 10); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("TaskTailFor foreign: %v", err)
	}
	if _, _, err := a.StopTaskFor(foreign, task.ID, tools.StopByUser); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("StopTaskFor foreign: %v", err)
	}
	if err := a.DeleteCronFor(foreign, cronID); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("DeleteCronFor foreign: %v", err)
	}

	if _, _, _, _, err := a.ShellTailFor("sess-own", "999", 10); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("ShellTailFor unknown: %v", err)
	}
	if _, _, err := a.KillShellFor("sess-own", "999", tools.StopByUser); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("KillShellFor unknown: %v", err)
	}
	if _, _, _, _, err := a.TaskTailFor("sess-own", "task-999", 10); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("TaskTailFor unknown: %v", err)
	}
	if _, _, err := a.StopTaskFor("sess-own", "task-999", tools.StopByUser); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("StopTaskFor unknown: %v", err)
	}
	if err := a.DeleteCronFor("sess-own", "cron-999"); !errors.Is(err, ErrActivityNotFound) {
		t.Errorf("DeleteCronFor unknown: %v", err)
	}

	// The foreign lookups must not have touched the items.
	snap := a.ActivityFor("sess-own")
	if len(snap.Shells) != 1 || !snap.Shells[0].Running || len(snap.Tasks) != 1 || snap.Tasks[0].Status != TaskRunning || len(snap.Crons) != 1 {
		t.Fatalf("items disturbed by foreign access: %+v", snap)
	}

	// Settle the bare task before Close: it has no run goroutine to close
	// its exited channel, so Close's StopAll would wait out the full
	// settle bound for it.
	if _, _, err := a.tasks.StopFor("sess-own", task.ID, tools.StopByModel); err != nil {
		t.Fatal(err)
	}
}

func TestActivityObserverAggregatesAllManagers(t *testing.T) {
	a, reg := newActivityAgent(t)
	rec := &activityRecorder{}
	a.SetActivityObserver(rec.observe)

	so := tracing.WithRun(t.Context(), "run-o", "sess-o")
	sx := tracing.WithRun(t.Context(), "run-x", "sess-x")

	// todos: driven through the real TodoWrite tool.
	callTodoWrite(t, reg, so, tools.Todo{Content: "observed", Status: "pending"})
	// shells: a real short-lived shell per session (start + exit each fire).
	shellO, err := reg.Background().Start(so, "sleep 0.1", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	shellX, err := reg.Background().Start(sx, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	// tasks: start and terminal transition.
	task, err := a.tasks.start(so, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.StopTaskFor("sess-o", task.ID, tools.StopByModel); err != nil {
		t.Fatal(err)
	}
	// crons: add, fire (last_fired_at changes) and delete.
	cronID, err := a.cron.Add("sess-o", "n", "p", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	a.fireDueCron(time.Date(2026, 11, 7, 10, 30, 0, 0, time.Local))
	if err := a.DeleteCronFor("sess-o", cronID); err != nil {
		t.Fatal(err)
	}

	if _, err := reg.Background().WaitFor(t.Context(), "sess-o", shellO, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Background().WaitFor(t.Context(), "sess-x", shellX, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// The exit callbacks fire from the shells' wait goroutines — wait for
	// them with a bound instead of asserting immediately.
	waitForEvents := func(session string, kind ActivityKind, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if rec.count(session, kind) >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("events (%s, %s) = %d, want %d", session, kind, rec.count(session, kind), want)
	}
	waitForEvents("sess-o", ActivityShells, 2)
	waitForEvents("sess-x", ActivityShells, 2)

	// Exactly one callback per trigger, each carrying the affected session
	// (the hooks are wired once, at construction).
	if got := rec.count("sess-o", ActivityTodos); got != 1 {
		t.Errorf("todos events = %d, want 1", got)
	}
	if got := rec.count("sess-o", ActivityTasks); got != 2 { // start + stop
		t.Errorf("tasks events = %d, want 2", got)
	}
	if got := rec.count("sess-o", ActivityCrons); got != 3 { // add + fire + delete
		t.Errorf("crons events = %d, want 3", got)
	}
	if got := rec.total(); got != 10 {
		t.Errorf("total events = %d, want 10 (no double wiring)", got)
	}

	// nil uninstalls: further changes fire nothing.
	a.SetActivityObserver(nil)
	callTodoWrite(t, reg, so, tools.Todo{Content: "silent", Status: "pending"})
	if got := rec.total(); got != 10 {
		t.Fatalf("events after uninstall = %d, want 10", got)
	}
}

func TestActivityObserverFiresOutsideManagerLocks(t *testing.T) {
	a, reg := newActivityAgent(t)
	// The observer re-enters every manager (a full snapshot): if any hook
	// fired while its manager lock was held, this would deadlock.
	a.SetActivityObserver(func(session string, kind ActivityKind) {
		_ = a.ActivityFor(session)
	})
	so := tracing.WithRun(t.Context(), "run-l", "sess-l")

	run := func(name string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go fnWrapper(fn, done)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s change hook deadlocked against its manager lock", name)
		}
	}
	run("todos", func() { callTodoWrite(t, reg, so, tools.Todo{Content: "x", Status: "pending"}) })
	run("shells", func() {
		id, err := reg.Background().Start(so, "true", "", ".")
		if err != nil {
			t.Error(err)
			return
		}
		// Keep the exit callback inside the bounded section too.
		if _, err := reg.Background().WaitFor(context.WithoutCancel(so), "sess-l", id, 5*time.Second); err != nil {
			t.Error(err)
		}
	})
	run("tasks", func() {
		if _, err := a.tasks.start(so, "s", "d", "general"); err != nil {
			t.Error(err)
		}
	})
	run("crons", func() {
		if _, err := a.cron.Add("sess-l", "n", "p", "* * * * *"); err != nil {
			t.Error(err)
		}
	})
}

// fnWrapper runs fn and closes done when it returns, so a hanging fn is
// visible to the bounded select instead of hanging the test binary.
func fnWrapper(fn func(), done chan struct{}) {
	fn()
	close(done)
}

func TestReleaseSessionStopsWorkAndSilencesTheSession(t *testing.T) {
	a, reg := newActivityAgent(t)
	rec := &activityRecorder{}
	a.SetActivityObserver(rec.observe)
	rel := tracing.WithRun(t.Context(), "run-rel", "sess-rel")

	shellID, err := reg.Background().Start(rel, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.tasks.start(rel, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.cron.Add("sess-rel", "n", "p", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	callTodoWrite(t, reg, rel, tools.Todo{Content: "doomed", Status: "pending"})
	a.QueueNotificationFor("sess-rel", "tag", "pending input")

	if before := rec.countForSession("sess-rel"); before != 4 {
		t.Fatalf("pre-release events = %d, want 4 (shells, tasks, crons, todos)", before)
	}

	rep := a.ReleaseSession("sess-rel")
	if rep.ShellsKilled != 1 || rep.TasksStopped != 1 || rep.CronsDeleted != 1 || !rep.TodosDropped {
		t.Fatalf("release report = %+v", rep)
	}

	// The killed shell's record is gone with the session (F4), so it settles
	// invisibly; its late completion must be dropped at the queue (D2), not
	// delivered.
	if _, _, err := reg.Background().OutputFor("sess-rel", shellID); !errors.Is(err, tools.ErrShellNotFound) {
		t.Fatalf("released shell record still resolvable: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the exit goroutine run its completion path
	if n := a.QueuedInputCountFor("sess-rel"); n != 0 {
		t.Fatalf("queued after release = %d, want 0 (late completions dropped)", n)
	}
	if got := rec.countForSession("sess-rel"); got != 4 {
		t.Fatalf("post-release events = %d, want 4 (released session stays silent)", got)
	}

	snap := a.ActivityFor("sess-rel")
	if len(snap.Crons) != 0 || snap.TodosKnown {
		t.Fatalf("crons/todos after release = %+v %+v", snap.Crons, snap.Todos)
	}
	// F4: the session's records were removed entirely — shells and tasks
	// alike, stopped or not.
	if len(snap.Tasks) != 0 {
		t.Fatalf("tasks after release = %+v", snap.Tasks)
	}
	if len(snap.Shells) != 0 {
		t.Fatalf("shells after release = %+v", snap.Shells)
	}
	if _, err := a.tasks.InfoFor("sess-rel", task.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("released task record still resolvable: %v", err)
	}

	// Releasing again, an unknown session and the unscoped session are all
	// no-ops with a zero report.
	if rep := a.ReleaseSession("sess-rel"); rep != (ReleaseReport{}) {
		t.Fatalf("re-release report = %+v, want zero", rep)
	}
	if rep := a.ReleaseSession("never-existed"); rep != (ReleaseReport{}) {
		t.Fatalf("unknown-session release report = %+v, want zero", rep)
	}
	if rep := a.ReleaseSession(""); rep != (ReleaseReport{}) {
		t.Fatalf("unscoped release report = %+v, want zero", rep)
	}
}

// countForSession counts events for one session, any kind.
func (r *activityRecorder) countForSession(session string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.session == session {
			n++
		}
	}
	return n
}

// TestQueueAddTaggedRejectsReleasedUnderLock pins the primitive's atomicity
// contract (round-1 review fix): the released decision is made INSIDE the
// queue's critical section, so both serial orderings around a mark+drop
// leave nothing behind — an item appended before the drop is removed by it,
// and an item arriving after the drop is refused outright. The genuinely
// concurrent interleaving is covered by TestReleasedDropAtomicWithQueueing.
func TestQueueAddTaggedRejectsReleasedUnderLock(t *testing.T) {
	q := &queue{}
	marked := false
	pred := func(string) bool { return marked }

	// Ordering 1: append lands before the mark; dropScope then removes it.
	if !q.addTagged(QueuedInput{Text: "a", Tag: "t", Session: "s"}, pred) {
		t.Fatal("pre-mark item refused")
	}
	marked = true
	if dropped := q.dropScope("s"); dropped != 1 {
		t.Fatalf("dropScope removed %d, want 1", dropped)
	}
	if n := q.count("s"); n != 0 {
		t.Fatalf("%d items survived dropScope", n)
	}

	// Ordering 2: append attempts after the mark are refused under the lock.
	if q.addTagged(QueuedInput{Text: "b", Tag: "t", Session: "s"}, pred) {
		t.Fatal("item queued for a released session")
	}
	if n := q.count("s"); n != 0 {
		t.Fatalf("released session holds %d items", n)
	}
	// Blank text is still dropped, predicate or not.
	if q.addTagged(QueuedInput{Text: "  ", Tag: "t", Session: "s"}, pred) {
		t.Fatal("blank text queued")
	}
	// Unscoped items never consult the predicate.
	if !q.addTagged(QueuedInput{Text: "u", Tag: "t"}, pred) {
		t.Fatal("unscoped item refused")
	}
}

// TestReleasedDropAtomicWithQueueing is the end-to-end regression for the
// TOCTOU the round-1 review found: queueNotification's released check and
// its addTagged append were two critical sections, so a completion passing
// the check just before ReleaseSession's mark could append after
// dropScope — leaving a queued item (and a notifyObserver fire) for a
// deleted session, which D2 forbids.
//
// Approach: hammer, not a barrier. The fix removes the check-then-queue
// window entirely (the decision moved inside addTagged's critical
// section), so a barrier between check and queue cannot be written against
// the fixed shape — there is nothing left to pause between. The hammer
// races several completions against one release per iteration, under the
// race detector; it was verified to fail on the pre-fix code before the
// fix was applied.
func TestReleasedDropAtomicWithQueueing(t *testing.T) {
	a, _ := newActivityAgent(t)
	const iterations = 200
	const adders = 4
	for i := 0; i < iterations; i++ {
		session := fmt.Sprintf("sess-race-%d", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1 + adders)
		for j := 0; j < adders; j++ {
			go func() {
				defer wg.Done()
				<-start
				a.queueNotification(QueuedInput{Text: "late completion", Tag: "background-task-completed", Session: session})
			}()
		}
		go func() {
			defer wg.Done()
			<-start
			a.ReleaseSession(session)
		}()
		close(start)
		wg.Wait()
		if n := a.QueuedInputCountFor(session); n != 0 {
			t.Fatalf("iteration %d: %d queued items survived release (TOCTOU regression)", i, n)
		}
	}
}

// TestEmitActivityReleaseRaceCannotQueueOrWake pins the documented
// best-effort contract of a manager change racing ReleaseSession (D2, spec
// §1 item 3). A real completion drives two seams at once: the notification
// queues (queueNotification) and the observer fan-out fires (emitActivity).
// The QUEUE half is atomic against the release — the released decision
// lives inside addTagged's critical section, and dropScope removes anything
// that appended before the mark — so the race can never leave a queued or
// wakeable item behind. The OBSERVER half is deliberately best-effort: a
// call that read observer+silenced just before the mark may still fire
// once after the release (that window cannot be closed without calling
// user code under a lock), which is harmless exactly because the wake
// drivers gate on the queue (WakeableCountFor), never on the observer.
// This test hammers the race under the detector and asserts what IS
// guaranteed: no queued item, no wakeable count — during the race by the
// atomicity above, and after it because the mark silences both seams
// absolutely. Spurious in-race observer fires are permitted and therefore
// not asserted to be zero.
func TestEmitActivityReleaseRaceCannotQueueOrWake(t *testing.T) {
	a, _ := newActivityAgent(t)
	rec := &activityRecorder{}
	a.SetActivityObserver(rec.observe)
	notifyFired := make(map[string]int)
	var notifyMu sync.Mutex
	a.SetNotificationObserver(func(q QueuedInput) {
		notifyMu.Lock()
		notifyFired[q.Session]++
		notifyMu.Unlock()
	})
	const iterations = 200
	const producers = 4
	for i := 0; i < iterations; i++ {
		session := fmt.Sprintf("sess-obs-race-%d", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1 + producers)
		for j := 0; j < producers; j++ {
			go func() {
				defer wg.Done()
				<-start
				// One completion-shaped unit: queue the notification, then
				// fire the manager-change observer — the same two steps a
				// background shell's exit performs.
				a.queueNotification(QueuedInput{
					Text: "racing completion", Tag: "background-task-completed", Session: session,
				})
				a.emitActivity(session, ActivityShells)
			}()
		}
		go func() {
			defer wg.Done()
			<-start
			a.ReleaseSession(session)
		}()
		close(start)
		wg.Wait()

		// Guaranteed invariants, checked immediately after the join.
		if n := a.QueuedInputCountFor(session); n != 0 {
			t.Fatalf("iteration %d: %d queued items survived the release race", i, n)
		}
		if n := a.WakeableCountFor(session); n != 0 {
			t.Fatalf("iteration %d: %d wakeable items survived the release race", i, n)
		}
		// From the mark on, both seams are silent absolutely: neither the
		// activity observer nor the notification observer fires again, and
		// a late completion queues nothing.
		obsBefore := rec.countForSession(session)
		notifyMu.Lock()
		notifBefore := notifyFired[session]
		notifyMu.Unlock()
		a.emitActivity(session, ActivityShells)
		a.queueNotification(QueuedInput{Text: "late", Tag: "background-task-completed", Session: session})
		if got := rec.countForSession(session); got != obsBefore {
			t.Fatalf("iteration %d: activity observer fired %d time(s) after the release mark", i, got-obsBefore)
		}
		notifyMu.Lock()
		notifAfter := notifyFired[session]
		notifyMu.Unlock()
		if notifAfter != notifBefore {
			t.Fatalf("iteration %d: notification observer fired %d time(s) for a refused late item", i, notifAfter-notifBefore)
		}
		if n := a.QueuedInputCountFor(session); n != 0 {
			t.Fatalf("iteration %d: %d late completions queued for a released session", i, n)
		}
	}
}

// F4: ReleaseSession removes the session's records entirely — finished or
// running, shells and tasks alike — while unscoped ("") records and the
// released mark itself (R2) survive.
func TestReleaseSessionRemovesRecords(t *testing.T) {
	a, reg := newActivityAgent(t)
	rel := tracing.WithRun(t.Context(), "run-rec", "sess-rec")

	shellID, err := reg.Background().Start(rel, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, st, _ := reg.Background().OutputFor("sess-rec", shellID); !st.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, err := a.tasks.start(rel, "subject", "prompt", "general")
	if err != nil {
		t.Fatal(err)
	}
	task.setFinalIfRunning(TaskCompleted, "done")
	a.tasks.notifyDone(task)
	if _, err := a.cron.Add("sess-rec", "n", "p", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	// An unscoped shell must survive the release untouched.
	unscopedID, err := reg.Background().Start(t.Context(), "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}

	rep := a.ReleaseSession("sess-rec")
	if rep.ShellsKilled != 0 || rep.TasksStopped != 0 || rep.CronsDeleted != 1 || rep.TodosDropped {
		t.Fatalf("release report = %+v", rep)
	}

	if _, _, err := reg.Background().OutputFor("sess-rec", shellID); !errors.Is(err, tools.ErrShellNotFound) {
		t.Fatalf("released shell record still resolvable: %v", err)
	}
	if _, err := a.tasks.InfoFor("sess-rec", task.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("released task record still resolvable: %v", err)
	}
	snap := a.ActivityFor("sess-rec")
	// The snapshot still shows unscoped items (visibility rule), but nothing
	// the session owns.
	for _, st := range snap.Shells {
		if st.Session == "sess-rec" {
			t.Fatalf("released shell in snapshot: %+v", st)
		}
	}
	for _, info := range snap.Tasks {
		if info.Session == "sess-rec" {
			t.Fatalf("released task in snapshot: %+v", info)
		}
	}
	if len(snap.Crons) != 0 {
		t.Fatalf("released session crons in snapshot: %+v", snap.Crons)
	}
	if st, err := reg.Background().StatusFor("", unscopedID); err != nil || st.Session != "" {
		t.Fatalf("unscoped shell affected by release: err=%v status=%+v", err, st)
	}

	// R2: the tombstone stays — a late completion for the released session is
	// refused, never queued.
	a.QueueNotificationFor("sess-rec", "tag", "late")
	if n := a.QueuedInputCountFor("sess-rec"); n != 0 {
		t.Fatalf("queued after release = %d, want 0", n)
	}
}
