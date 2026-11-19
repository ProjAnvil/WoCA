package agent

// Task 7: the session-scoped subagent-runs registry — the concurrency cap's
// accounting unit and the lifecycle-event source (graph-native HITL
// §3.2/§3.7). route2's rejection behavior is pinned in
// TestSubgraphCapRejects (subgraphfanout_test.go); these tests cover the
// registry in isolation.

import (
	"slices"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"

	"github.com/projanvil/woca/internal/tools"
)

func testRun(session, callID string) SubagentRun {
	return SubagentRun{
		Def:     "reviewer",
		CallID:  callID,
		Session: session,
		Origin:  "subagent:reviewer#" + callID,
	}
}

func TestSubagentRunsLifecycle(t *testing.T) {
	r := newSubagentRuns(0) // 0 → the default cap
	if r.capLimit() != defaultMaxSubagents {
		t.Fatalf("capLimit = %d, want the default %d", r.capLimit(), defaultMaxSubagents)
	}
	var events []SubagentEvent
	r.SetOnEvent(func(session string, ev SubagentEvent) { events = append(events, ev) })
	run := testRun("s1", "t1")

	if !r.start(run) {
		t.Fatal("first start must register")
	}
	if r.start(run) {
		t.Fatal("re-entry start must be idempotent")
	}
	if got := r.activeCount("s1"); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}

	// Waiting flips state without a second entry; resume flips it back.
	r.waiting(run)
	r.start(run)
	snap := r.snapshotFor("s1")
	if len(snap) != 1 || snap[0].State != SubagentRunning {
		t.Fatalf("snapshot after resume = %+v, want one running entry", snap)
	}

	// Terminal exactly once: a racing stop cannot override done.
	r.waiting(run)
	if !r.finish(run, SubagentDone) {
		t.Fatal("finish from waiting must transition")
	}
	if r.finish(run, SubagentStopped) {
		t.Fatal("a second finish must not override the terminal state")
	}
	if got := r.activeCount("s1"); got != 0 {
		t.Fatalf("active after done = %d, want 0", got)
	}
	snap = r.snapshotFor("s1")
	if snap[0].State != SubagentDone {
		t.Fatalf("state = %s, want done", snap[0].State)
	}

	// The event trail: started, waiting, running, waiting, done.
	wantStates := []SubagentRunState{SubagentRunning, SubagentWaiting, SubagentRunning, SubagentWaiting, SubagentDone}
	if len(events) != len(wantStates) {
		t.Fatalf("events = %+v, want %d entries", events, len(wantStates))
	}
	for i, ev := range events {
		if ev.State != wantStates[i] || ev.CallID != "t1" || ev.Session != "s1" || ev.Def != "reviewer" {
			t.Fatalf("event %d = %+v, want state %s for reviewer#t1@s1", i, ev, wantStates[i])
		}
	}
}

func TestSubagentRunsSessionScoped(t *testing.T) {
	r := newSubagentRuns(8)
	r.start(testRun("s1", "t1"))
	r.start(testRun("s2", "t1")) // same call id, other session: distinct entry
	if got := r.activeCount("s1"); got != 1 {
		t.Fatalf("s1 active = %d, want 1", got)
	}
	if got := r.activeCount("s2"); got != 1 {
		t.Fatalf("s2 active = %d, want 1", got)
	}
	if got := len(r.snapshotFor("s1")); got != 1 {
		t.Fatalf("s1 snapshot = %d entries, want 1", got)
	}
}

func TestSubagentRunsStopSession(t *testing.T) {
	r := newSubagentRuns(8)
	r.start(testRun("s1", "t1"))
	r.start(testRun("s1", "t2"))
	r.waiting(testRun("s1", "t3")) // waiting children stop too
	done := testRun("s1", "t0")
	r.start(done)
	r.finish(done, SubagentDone)

	if n := r.stopSession("s1"); n != 3 {
		t.Fatalf("stopped %d, want 3 (running and waiting, not done)", n)
	}
	if got := r.activeCount("s1"); got != 0 {
		t.Fatalf("active after stopSession = %d, want 0", got)
	}
	states := map[string]SubagentRunState{}
	for _, run := range r.snapshotFor("s1") {
		states[run.CallID] = run.State
	}
	if states["t1"] != SubagentStopped || states["t2"] != SubagentStopped || states["t3"] != SubagentStopped {
		t.Fatalf("states = %v, want t1/t2/t3 stopped", states)
	}
	if states["t0"] != SubagentDone {
		t.Fatalf("done entry was disturbed: %v", states)
	}
	// Idempotent.
	if n := r.stopSession("s1"); n != 0 {
		t.Fatalf("second stopSession stopped %d, want 0", n)
	}
}

// TestSubagentsCapFromConfig: the assembly-level wiring of D12 — agent.New
// seeds the runs registry from Config.MaxConcurrentSubagents (the
// subagents.max-concurrent settings value injected at assembly), falling
// back to the hard default when unset.
func TestSubagentsCapFromConfig(t *testing.T) {
	m := language.NewFakeChatModel()
	a, err := New(Config{
		Model:                  m,
		Registry:               tools.NewRegistry(t.TempDir()),
		MaxConcurrentSubagents: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.SubagentsCap(); got != 3 {
		t.Fatalf("SubagentsCap = %d, want 3 (the configured cap)", got)
	}

	b, err := New(Config{Model: m, Registry: tools.NewRegistry(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.SubagentsCap(); got != defaultMaxSubagents {
		t.Fatalf("default SubagentsCap = %d, want %d", got, defaultMaxSubagents)
	}
}

func TestSubagentRunsFinishedRecordsCapped(t *testing.T) {
	r := newSubagentRuns(8)
	for i := range 32 {
		run := testRun("s1", string(rune('a'+i)))
		r.start(run)
		r.finish(run, SubagentDone)
	}
	snap := r.snapshotFor("s1")
	if len(snap) > tools.MaxFinishedRecordsPerSession {
		t.Fatalf("%d finished records survived, want at most %d", len(snap), tools.MaxFinishedRecordsPerSession)
	}
	if !slices.ContainsFunc(snap, func(run SubagentRun) bool { return run.CallID == string(rune('a'+31)) }) {
		t.Fatalf("newest record pruned; snapshot: %+v", snap)
	}
}
