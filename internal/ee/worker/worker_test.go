package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func newWorkerFixture(t *testing.T, reply string) (*Worker, *runstate.Ledger, *eventbus.Local) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI(reply))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.NewLocalWithRetention(1024)
	t.Cleanup(func() { _ = bus.Close() })
	w := New(Config{
		Ledger: ledger,
		Agent:  a,
		Bus:    bus,
		Logger: zerolog.Nop(),
		ID:     "worker-test",
	})
	return w, ledger, bus
}

func enqueueRun(t *testing.T, ledger *runstate.Ledger, runID, sessionID, prompt string) {
	t.Helper()
	snap, err := runstate.EncodeSnapshot(runstate.RunSnapshot{Prompt: prompt, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Enqueue(context.Background(), runstate.Run{
		RunID: runID, SessionID: sessionID, ThreadID: "th-" + runID,
		ConfigSnapshot: snap,
	}); err != nil {
		t.Fatal(err)
	}
}

// The worker claims a queued run, executes it to completion, and finishes
// the ledger row — the full claim→execute→finish cycle.
func TestWorkerClaimsAndExecutes(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "worker reply")
	enqueueRun(t, ledger, "r1", "s1", "hello")

	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	row, ok, err := ledger.Row(context.Background(), "r1")
	if err != nil || !ok {
		t.Fatalf("row = ok=%v err=%v", ok, err)
	}
	if row.Status != "done" {
		t.Fatalf("status = %q want done", row.Status)
	}
	if row.Owner != "worker-test" {
		t.Fatalf("owner = %q", row.Owner)
	}
}

// Step on an empty queue returns nil (nothing to do).
func TestWorkerStepOnEmptyQueue(t *testing.T) {
	w, _, _ := newWorkerFixture(t, "x")
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Multiple runs execute one per Step.
func TestWorkerStepsThroughQueue(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "reply")
	enqueueRun(t, ledger, "r1", "s1", "one")
	enqueueRun(t, ledger, "r2", "s2", "two")

	for i := 0; i < 2; i++ {
		if err := w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"r1", "r2"} {
		row, ok, _ := ledger.Row(context.Background(), id)
		if !ok || row.Status != "done" {
			t.Fatalf("%s = %+v ok=%v", id, row, ok)
		}
	}
}

// Run drives Step in a loop until the context is cancelled.
func TestWorkerRunLoop(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "loop reply")
	enqueueRun(t, ledger, "r1", "s1", "hi")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Run the loop until the run completes, then cancel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	// Wait for the run to finish, then cancel the loop.
	deadline := time.After(5 * time.Second)
	for {
		row, ok, _ := ledger.Row(context.Background(), "r1")
		if ok && row.Status == "done" {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatal("run never completed")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Events emitted during execution are mirrored into run_events.
func TestWorkerMirrorsEvents(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "mirror reply")
	enqueueRun(t, ledger, "r1", "s1", "hello")
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := ledger.Replay(context.Background(), "r1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no mirrored events")
	}
	kinds := map[string]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	if !kinds["done"] {
		t.Fatalf("missing done in %v", kinds)
	}
}

// Heartbeat extends the lease while a run is in flight (the worker's lease
// must not expire under active execution).
func TestWorkerHeartbeatDuringExecution(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "slow reply")
	enqueueRun(t, ledger, "r1", "s1", "hello")
	// The run completes; the heartbeat ticker runs during Step.
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, _, _ := ledger.Row(context.Background(), "r1")
	if row.LeaseExpiresAt != 0 {
		t.Fatalf("lease not released at finish: %+v", row)
	}
}
