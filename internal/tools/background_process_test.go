//go:build unix

package tools

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTreeShell starts a background shell whose command echoes pids to
// stdout, and returns once at least minPids of them are visible in the
// accumulated output. Cleanup kills the reported pids directly so a failing
// test never leaks sleepers.
func startTreeShell(t *testing.T, minPids int, command string) (bg *BackgroundShells, id string, pids []int) {
	t.Helper()
	bg = NewBackgroundShells()
	id, err := bg.Start(context.Background(), command, "", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err := bg.OutputFor("", id)
		if err != nil {
			t.Fatal(err)
		}
		if pids = parsePids(out); len(pids) >= minPids {
			t.Cleanup(func() {
				for _, pid := range pids {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			return bg, id, pids
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell %q never reported %d pids", command, minPids)
	return nil, "", nil
}

// parsePids collects every whitespace-separated integer in out.
func parsePids(out string) []int {
	var pids []int
	for _, f := range strings.Fields(out) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// pidGone reports whether pid no longer exists (ESRCH).
func pidGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// TestKillForTerminatesProcessTree reproduces defect N3 through the manager:
// a killed background shell must leave the running state within a bound and
// its grandchildren must be gone — before the fix only the bash process died
// while the children kept the output pipe open, so the shell stayed
// "running" forever.
func TestKillForTerminatesProcessTree(t *testing.T) {
	bg, id, pids := startTreeShell(t, 2, "sleep 300 & a=$!; sleep 300 & echo $a $!; wait")
	if len(pids) != 2 {
		t.Fatalf("want 2 child pids, got %v", pids)
	}
	if _, alreadyFinished, err := bg.KillFor("", id, StopByUser); err != nil || alreadyFinished {
		t.Fatalf("KillFor = (alreadyFinished %v, err %v)", alreadyFinished, err)
	}
	st, err := bg.WaitFor(context.Background(), "", id, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Fatal("background shell still running 8s after KillFor: its process tree survived the kill")
	}
	if !st.Killed || st.StoppedBy != StopByUser {
		t.Fatalf("stop facts = killed %v by %q", st.Killed, st.StoppedBy)
	}
	for _, pid := range pids {
		deadline := time.Now().Add(3 * time.Second)
		for !pidGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("grandchild %d survived KillFor", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestKillAllStopsEveryRunningShell verifies shutdown cleanup: every running
// shell is marked killed by the given origin and stops, while already
// finished shells keep their recorded state untouched.
func TestKillAllStopsEveryRunningShell(t *testing.T) {
	bg := NewBackgroundShells()
	t.Cleanup(func() { bg.KillAll(StopByUser) })

	slowA, err := bg.Start(context.Background(), "sleep 300", "", "")
	if err != nil {
		t.Fatal(err)
	}
	finished, err := bg.Start(context.Background(), "true", "", "")
	if err != nil {
		t.Fatal(err)
	}
	slowB, err := bg.Start(context.Background(), "sleep 300", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := bg.WaitFor(context.Background(), "", finished, 5*time.Second); err != nil || st.Running {
		t.Fatalf("finished shell: (%v, %v)", st, err)
	}

	bg.KillAll(StopByUser)

	for _, id := range []string{slowA, slowB} {
		st, err := bg.WaitFor(context.Background(), "", id, 8*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if st.Running {
			t.Fatalf("shell %s still running after KillAll", id)
		}
		if !st.Killed || st.StoppedBy != StopByUser {
			t.Fatalf("shell %s stop facts = killed %v by %q", id, st.Killed, st.StoppedBy)
		}
	}
	st, err := bg.StatusFor("", finished)
	if err != nil {
		t.Fatal(err)
	}
	if st.Killed || st.StoppedBy != "" || st.ExitCode != 0 {
		t.Fatalf("finished shell must keep its state: %+v", st)
	}
}

// TestKillAllWaitsForTermIgnoringTree pins the shutdown contract of KillAll:
// the SIGKILL escalation runs on a timer goroutine inside this process, so a
// KillAll that returns before the timer fires loses the escalation as soon
// as the caller exits (graceful shutdown exits right after Agent.Close) and
// a TERM-ignoring tree survives as an orphan. KillAll must wait out the
// escalation: when it returns, the tree is gone — and it must stay bounded.
func TestKillAllWaitsForTermIgnoringTree(t *testing.T) {
	// The trap is set before the fork so the sleep inherits the ignored
	// disposition: the group SIGTERM is a no-op for the whole tree.
	bg, id, pids := startTreeShell(t, 2, "trap '' TERM; echo $$; sleep 300 & echo $!; wait")

	start := time.Now()
	bg.KillAll(StopByUser)
	if elapsed := time.Since(start); elapsed > killSettleBound {
		t.Fatalf("KillAll blocked %s, beyond the settle bound %s", elapsed, killSettleBound)
	}
	st, err := bg.StatusFor("", id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Fatal("KillAll returned while a TERM-ignoring shell was still running: the SIGKILL escalation was lost")
	}
	if !st.Killed || st.StoppedBy != StopByUser {
		t.Fatalf("stop facts = killed %v by %q", st.Killed, st.StoppedBy)
	}
	// pids[0] is the shell itself (reaped before the shell settles); the
	// orphaned child may lag a moment in the reaper's queue.
	for i, pid := range pids {
		deadline := time.Now().Add(2 * time.Second)
		for !pidGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("pid %d (%s) alive after KillAll returned", pid, map[bool]string{true: "shell", false: "child"}[i == 0])
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
