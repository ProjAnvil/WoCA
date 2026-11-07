//go:build unix

package executor

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// newProcessGroup puts the child into its own process group (group id =
// child pid, inherited by every descendant) so the executor can signal the
// whole tree at once. A side effect: terminal Ctrl+C no longer reaches the
// children — shutdown paths must terminate them explicitly via Kill.
func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate signals the process group led by this process with SIGTERM and,
// unless Wait reports the process gone within killGrace, escalates to
// SIGKILL on the group. It returns without waiting; the escalation is
// driven by the timer goroutine, which stands down when Wait returns.
func (p *localProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil // never started; nothing to signal
	}
	p.termOnce.Do(func() {
		pid := p.cmd.Process.Pid
		go func() {
			t := time.NewTimer(killGrace)
			defer t.Stop()
			select {
			case <-p.exited:
				// Wait returned: the process is gone, escalation is moot.
			case <-t.C:
				// The group ignored or trapped SIGTERM — force it.
				_ = signalProcessGroup(pid, syscall.SIGKILL)
			}
		}()
	})
	return signalProcessGroup(p.cmd.Process.Pid, syscall.SIGTERM)
}

// signalProcessGroup delivers sig to the process group led by pid. A group
// that is already gone is success, not an error: kills racing with exit (or
// a second Kill) are expected and callers may ignore the result anyway.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
