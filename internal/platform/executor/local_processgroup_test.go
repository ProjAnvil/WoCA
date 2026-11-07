//go:build unix

package executor

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncedBuffer is a mutex-guarded bytes.Buffer: the exec copy goroutine
// writes while the test polls the contents.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startWithPids starts command under the Local executor with redirected
// stdout and returns once the command has echoed its own pid or its
// children's pids there (all whitespace-separated numbers on stdout are
// collected). Cleanup kills the process and the reported pids so a failing
// test never leaks sleepers.
func startWithPids(t *testing.T, command string) (Process, []int) {
	t.Helper()
	buf := &syncedBuffer{}
	p, err := NewLocal().Start(t.Context(),
		ExecRequest{Name: "sh", Args: []string{"-c", command}},
		StartOptions{Stdout: buf, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pids = parsePids(buf.String()); len(pids) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pids) == 0 {
		_ = p.Kill()
		t.Fatalf("command %q never reported pids (output %q)", command, buf.String())
	}
	t.Cleanup(func() {
		_ = p.Kill()
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return p, pids
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

// pidGone reports whether pid no longer exists (ESRCH); used to prove the
// whole tree died, not just the process the executor tracked.
func pidGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitUntil polls cond until it holds or bound elapses; every wait in these
// tests is bounded so a regression fails instead of hanging.
func waitUntil(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s within %s", what, bound)
}

// waitBounded waits for p with a hard bound so a broken Kill can never hang
// the test.
func waitBounded(t *testing.T, p Process, bound time.Duration) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- p.Wait() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(bound):
		t.Fatalf("Wait did not return within %s", bound)
		return nil
	}
}

// TestStartKillTerminatesProcessTree reproduces defect N3: a background
// command's children inherit the output pipe, so killing only the shell
// process leaves them running (holding ports) and keeps Wait blocked on the
// pipe forever. Kill must terminate the whole process group: Wait returns
// within a bound and the grandchildren are gone.
func TestStartKillTerminatesProcessTree(t *testing.T) {
	p, pids := startWithPids(t, "sleep 300 & a=$!; sleep 300 & echo $a $!; wait")
	if len(pids) != 2 {
		t.Fatalf("want 2 child pids, got %v", pids)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := waitBounded(t, p, 3*time.Second); err == nil {
		t.Fatal("killed process must report a non-nil wait error")
	}
	for _, pid := range pids {
		waitUntil(t, 3*time.Second, "grandchild survived Kill", func() bool { return pidGone(pid) })
	}
}

// TestStartKillEscalatesToSIGKILLWhenTermIgnored verifies the grace period:
// a shell that ignores SIGTERM (its children inherit the ignored
// disposition, so the group TERM is a no-op) must still die — Kill
// escalates to SIGKILL on the group after the grace elapses, well within
// grace + 1s.
func TestStartKillEscalatesToSIGKILLWhenTermIgnored(t *testing.T) {
	p, pids := startWithPids(t, "echo $$; trap '' TERM; sleep 300")
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := waitBounded(t, p, KillGrace+2*time.Second); err == nil {
		t.Fatal("killed process must report a non-nil wait error")
	}
	for _, pid := range pids {
		waitUntil(t, KillGrace+time.Second, "TERM-ignoring process survived Kill", func() bool { return pidGone(pid) })
	}
}

// TestStartWaitReturnsWhenGrandchildHoldsPipe covers the other half of N3
// without any Kill: the shell exits on its own but leaves a child holding
// the output pipe. Wait must give up on the pipe after the WaitDelay
// instead of blocking forever (the "running forever" state the shell
// manager would otherwise record).
func TestStartWaitReturnsWhenGrandchildHoldsPipe(t *testing.T) {
	p, _ := startWithPids(t, "sleep 300 & echo $!; exit 0")
	err := waitBounded(t, p, pipeWaitDelay+2*time.Second)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait error = %v, want exec.ErrWaitDelay", err)
	}
}
