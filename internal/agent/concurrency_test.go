package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/tools"
)

// Concurrency tests for the paths shared across HTTP handlers and runs.
// These reproduce the interleavings the plain unit tests miss: restore-time
// goal notification, concurrent runs consuming memory prefetch, team status
// reads while teammates run, concurrent task claiming, and mid-run settings
// mutations (mode, tool refresh).

// TestRestoreGoalNotifiesWithoutDeadlock guards the restore path used after a
// server restart: RestoreGoal must notify subscribers after releasing the goal
// mutex, otherwise a subscriber taking the same mutex self-deadlocks.
func TestRestoreGoalNotifiesWithoutDeadlock(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	notified := make(chan *Goal, 1)
	lead.SubscribeGoal(func(g *Goal) { notified <- g })

	done := make(chan error, 1)
	go func() { done <- lead.RestoreGoal(&Goal{Objective: "ship it"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RestoreGoal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RestoreGoal deadlocked while notifying subscribers")
	}
	select {
	case g := <-notified:
		if g.Objective != "ship it" {
			t.Fatalf("notified objective = %q", g.Objective)
		}
	default:
		t.Fatal("RestoreGoal did not notify subscribers")
	}
}

// TestTakeMemoryAttachmentsConcurrentRuns mimics two concurrent runs
// consuming the same agent's prefetch state: the injected-key map is shared
// agent state, so concurrent takes must not mutate it outside the mutex
// (Go fatals on concurrent map writes; -race flags the mutation).
func TestTakeMemoryAttachmentsConcurrentRuns(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	const rounds = 100
	for i := 0; i < rounds; i++ {
		hits := make([]memory.Hit, 6)
		for j := range hits {
			hits[j] = memory.Hit{ID: fmt.Sprintf("round-%d-hit-%d", i, j), Summary: "s", Content: "c"}
		}
		lead.mem.mu.Lock()
		lead.mem.prefetch.engineHits = hits
		if lead.mem.prefetch.injected == nil {
			lead.mem.prefetch.injected = map[string]bool{"seed": true}
		}
		lead.mem.mu.Unlock()

		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lead.takeMemoryAttachments()
			}()
		}
		wg.Wait()
	}
}

// TestSetModeConcurrentReaders pairs the HTTP SetMode call with Mode readers
// as checkPermission sees them: both sides must synchronize on a.mu.
func TestSetModeConcurrentReaders(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			lead.SetMode(config.ModeAcceptEdits)
			lead.SetMode(config.ModeBypass)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = lead.Mode()
		}
	}()
	wg.Wait()
}

// extraTool toggles between two tool sets so RefreshToolsNow rebuilds.
func extraTools(gen int) []coretools.Tool {
	name := fmt.Sprintf("extra-%d", gen%2)
	tool, err := coretools.FromFunc(name, "test extra tool", func(ctx context.Context, _ struct{}) (coretools.Result, error) {
		return coretools.Result{Content: "ok"}, nil
	})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{tool}
}

// TestRefreshToolsConcurrentReaders exercises the plugin-toggle path: HTTP
// RefreshToolsNow rebuilding the tool set while a run's readers (ToolNames,
// execute's by-name lookup) iterate the same fields.
func TestRefreshToolsConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		WorkDir:        dir,
		PermissionMode: config.ModeBypass,
		RefreshTools: func(ctx context.Context) []coretools.Tool {
			return extraTools(time.Now().Nanosecond())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.RefreshToolsNow(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = a.ToolNames()
		}
	}()
	wg.Wait()
}

// TestClaimNextConcurrentAllClaimed has as many concurrent claimers as tasks:
// a claimer that loses the atomic Claim race on its first candidate must keep
// scanning instead of reporting "nothing to do".
func TestClaimNextConcurrentAllClaimed(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	team := lead.ensureTeam()
	const n = 8
	for i := 0; i < n; i++ {
		team.list.Create(fmt.Sprintf("task %d", i), "", "")
	}
	var mu sync.Mutex
	claimed := map[string]string{} // task ID -> owner
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			rec, ok := team.claimNext(owner)
			if ok {
				mu.Lock()
				claimed[rec.ID] = owner
				mu.Unlock()
			}
		}(fmt.Sprintf("mate-%d", i))
	}
	wg.Wait()
	if len(claimed) != n {
		t.Fatalf("claimed %d of %d tasks concurrently; a losing claimer bailed instead of scanning on", len(claimed), n)
	}
}

// TestReleaseOwnedKeepsCompletedTask pins the release semantics: a task the
// lead marked completed after the teammate's snapshot must not be reverted to
// pending by releaseOwned (Release must re-check owner and status in the
// TaskList lock).
func TestReleaseOwnedKeepsCompletedTask(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	team := lead.ensureTeam()
	rec := team.list.Create("doomed", "", "")
	if _, ok := team.list.Claim(rec.ID, "alice"); !ok {
		t.Fatal("claim failed")
	}
	// The lead completes the task after alice's snapshot, then alice errors.
	if _, ok := team.list.Update(rec.ID, TaskRecordCompleted, "", ""); !ok {
		t.Fatal("update failed")
	}
	team.releaseOwned("alice")
	got, _ := team.list.Get(rec.ID)
	if got.Status != TaskRecordCompleted {
		t.Fatalf("releaseOwned reverted a completed task: status=%s owner=%s", got.Status, got.Owner)
	}
}

// TestTeamStatusConcurrentUpdates pairs Status() with the locked field writes
// run() performs: the formatter must snapshot member fields under the team
// mutex instead of reading them unlocked.
func TestTeamStatusConcurrentUpdates(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	team := lead.ensureTeam()
	m := &teammate{name: "alice", agent: lead, status: teammateIdle}
	team.mu.Lock()
	team.members["alice"] = m
	team.order = append(team.order, "alice")
	team.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			team.mu.Lock()
			m.status = teammateWorking
			m.note = fmt.Sprintf("note %d", i)
			m.turns = i
			team.mu.Unlock()
		}
	}()
	for i := 0; i < 500; i++ {
		_ = team.Status()
	}
	wg.Wait()
}

// TestTeamSpawnStopConcurrent stresses the Spawn/Stop pair: cancel must be
// published under the team mutex (Stop on another goroutine reads it while
// the member is already visible).
func TestTeamSpawnStopConcurrent(t *testing.T) {
	lead, _ := newTeamLead(t, teammateModelScript())
	team := lead.ensureTeam()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			name := fmt.Sprintf("mate-%d", i)
			if err := team.Spawn(ctx, name, "You do things.", nil); err != nil {
				return // later iterations may race a duplicate; fine
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = team.Stop(fmt.Sprintf("mate-%d", i), "")
			_ = team.Status()
		}
	}()
	wg.Wait()
	team.Close()
}

// TestBackgroundTaskStopBeatsLateCompletion pins the TaskStop semantics: once
// a task is stopped, the background goroutine's late completion must not flip
// the status back to completed.
func TestBackgroundTaskStopBeatsLateCompletion(t *testing.T) {
	tm := newTaskManager()
	task := tm.start("subject", "description")
	task.stop()
	// The background goroutine reports completion through setFinalIfRunning;
	// after TaskStop that report must be dropped.
	task.setFinalIfRunning(TaskCompleted, "late report")
	status, _, final := task.snapshot()
	if status != TaskStopped {
		t.Fatalf("late completion overrode stopped status: %s", status)
	}
	if final != "" {
		t.Fatalf("late completion overrode stopped report: %q", final)
	}
}

// TestAbandonGoalRunReleasesStuckRun pins the runner-exit contract: an
// abandoned goal run (error, cancellation, an interrupt nobody answers) must
// release the "running" bookkeeping so a later GoalStream is not refused
// with "already running", while a stale abandon must not kill a newer run.
func TestAbandonGoalRunReleasesStuckRun(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if _, err := lead.SetGoal("stuck goal", "", 0, 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s1, err := lead.GoalStream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	run1 := lead.GoalRunID()
	if run1 == 0 {
		t.Fatal("expected a run id in flight")
	}
	// Simulate the runner exiting without draining to a terminal step.
	lead.AbandonGoalRun(run1)
	if lead.GoalRunning() {
		t.Fatal("abandoned run still marked running")
	}
	_ = s1.Close()

	// A newer run must ignore the stale abandon.
	s2, err := lead.GoalStream(ctx, nil)
	if err != nil {
		t.Fatalf("re-stream after abandon: %v", err)
	}
	run2 := lead.GoalRunID()
	lead.AbandonGoalRun(run1) // stale id: no effect on run2
	if !lead.GoalRunning() {
		t.Fatal("stale abandon killed the newer run")
	}
	lead.AbandonGoalRun(run2)
	_ = s2.Close()
}
