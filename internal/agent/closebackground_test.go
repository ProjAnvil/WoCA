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
