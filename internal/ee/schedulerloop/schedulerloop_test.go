package schedulerloop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// newDeps builds RunLoopDeps over an isolated settings database: the ledger
// rides the store's DB exactly like the serve path builds it, and the agent
// answers from a fake model so an executed run completes deterministically.
func newDeps(t *testing.T, reply string) (servefront.RunLoopDeps, *runstate.Ledger) {
	t.Helper()
	dir := t.TempDir()
	st, err := settings.Open(filepath.Join(dir, "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ledger, err := runstate.New(st.DB())
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
	return servefront.RunLoopDeps{
		Agent:      a,
		Settings:   st,
		Ledger:     ledger,
		SessionDir: filepath.Join(dir, "sessions"),
		Log:        zerolog.Nop(),
	}, ledger
}

func enqueueRun(t *testing.T, ledger *runstate.Ledger, runID, sessionID, prompt string) {
	t.Helper()
	snap, err := runstate.EncodeSnapshot(runstate.RunSnapshot{Prompt: prompt, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: runID, SessionID: sessionID, ThreadID: "th-" + runID,
		ConfigSnapshot: snap,
	}); err != nil {
		t.Fatal(err)
	}
}

// runLoop drives RunLoop in its own goroutine (the serve path does the same)
// and returns a channel that closes when the loop exits.
func runLoop(ctx context.Context, deps servefront.RunLoopDeps) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLoop(ctx, deps)
	}()
	return done
}

// The loop claims an enqueued run through the full assembly — sessions,
// states, secrets, settings — and drives it to a terminal ledger row.
func TestRunLoopExecutesEnqueuedRun(t *testing.T) {
	deps, ledger := newDeps(t, "scheduler reply")
	enqueueRun(t, ledger, "r1", "s1", "hello")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := runLoop(ctx, deps)
	deadline := time.After(10 * time.Second)
	for {
		row, ok, err := ledger.Row(t.Context(), "r1")
		if err != nil {
			t.Fatal(err)
		}
		if ok && row.Status == "done" {
			if row.Owner == "" {
				t.Fatalf("owner empty: %+v", row)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("enqueued run never completed")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop did not exit after context cancel")
	}
}

// An empty queue parks the loop without panicking (nil plane keeps the poll
// cadence) and a cancelled context ends it promptly.
func TestRunLoopIdlesOnEmptyQueueAndStopsOnCancel(t *testing.T) {
	deps, _ := newDeps(t, "x")

	ctx, cancel := context.WithCancel(t.Context())
	done := runLoop(ctx, deps)
	// Let the loop idle through a few empty claim attempts.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop did not exit after context cancel")
	}
}

// Without a ledger (no settings database: the serve path leaves deps.Ledger
// nil) the loop declines to start instead of spinning on a nil claim source.
func TestRunLoopSkipsWithoutLedger(t *testing.T) {
	done := runLoop(t.Context(), servefront.RunLoopDeps{Log: zerolog.Nop()})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop without a ledger never returned (should not start)")
	}
}

// The run-loop identity is host-unique (hostname + pid): two schedulers on
// one host, or container deployments where every process is pid 1, must not
// share a lease identity.
func TestSchedulerIDShape(t *testing.T) {
	id := schedulerID()
	if !strings.HasPrefix(id, "sched-") {
		t.Fatalf("id = %q, want sched-<host>-<pid>", id)
	}
	if !strings.HasSuffix(id, fmt.Sprintf("-%d", os.Getpid())) {
		t.Fatalf("id = %q, want the pid suffix", id)
	}
}
