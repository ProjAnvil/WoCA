//go:build unix

package agent

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// TestAgentCloseKillsBackgroundShells reproduces defect N6: background
// shells run detached from every context, so Agent.Close must terminate
// them; after Close the shell has left the running state and its child
// process is gone.
func TestAgentCloseKillsBackgroundShells(t *testing.T) {
	reg := tools.NewRegistry(t.TempDir())
	a, err := New(Config{
		Model:          language.NewFakeChatModel(),
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	id, err := reg.Background().Start(context.Background(), "sleep 300 & echo $!; wait", "", "")
	if err != nil {
		t.Fatal(err)
	}
	pid := 0
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _, err := reg.Background().OutputFor("", id)
		if err != nil {
			t.Fatal(err)
		}
		if fields := strings.Fields(out); len(fields) > 0 {
			if p, err := strconv.Atoi(fields[0]); err == nil {
				pid = p
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("shell never reported its child pid")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	a.Close()

	st, err := reg.Background().WaitFor(context.Background(), "", id, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Fatal("background shell still running 8s after Agent.Close")
	}
	if !st.Killed || st.StoppedBy != tools.StopByUser {
		t.Fatalf("stop facts = killed %v by %q", st.Killed, st.StoppedBy)
	}
	deadline = time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatalf("background child %d survived Agent.Close", pid)
	}
}

// TestAgentCloseStopsDetachedBackgroundTasks pins the Close contract for
// detached background subagents (D3): they no longer die with the launching
// run's ctx, so Agent.Close must stop them explicitly — like the shells'
// KillAll — with user attribution, and their run goroutines must wind down
// within a bounded settle instead of surviving the session as orphans that
// keep spending model calls (the SDK calls Close inside long-lived hosts).
func TestAgentCloseStopsDetachedBackgroundTasks(t *testing.T) {
	gate := make(chan struct{})
	sub := &entryGatedModel{FakeChatModel: language.NewFakeChatModel(), gate: gate}
	a := newBackgroundTaskAgent(t, sub)

	notif := make(chan string, 1)
	a.tasks.SetNotify(func(session, text string, stoppedBy tools.StopOrigin) { notif <- text })

	// The launching run ends (ctx cancelled) while the task is mid-run; the
	// D3 detach keeps the task alive, so only Close can stop it now.
	runCtx, cancelRun := context.WithCancel(tracing.WithRun(context.Background(), "run-c", "sess-c"))
	if _, err := a.runTask(runCtx, taskArgs{Description: "orphan risk", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	cancelRun()

	a.Close()

	info, err := a.tasks.InfoFor("sess-c", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != TaskStopped || info.StoppedBy != tools.StopByUser {
		t.Fatalf("stop facts after Close = %+v, want stopped/user", info)
	}
	// The run goroutine wound down within StopAll's bounded settle: the
	// completion notification (fired by the goroutine itself) has landed.
	select {
	case text := <-notif:
		if !strings.Contains(text, "stopped") {
			t.Fatalf("post-Close notification = %q, want the stopped status line", text)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("background task goroutine still alive 8s after Agent.Close")
	}
	close(gate) // release the subagent if the stop never reached it
}
