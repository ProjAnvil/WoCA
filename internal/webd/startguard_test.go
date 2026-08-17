package webd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newGuardManager builds a manager whose Spawn/Kill/PortOwner are
// injectable, rooted at a temp dir, with a short ready timeout. WaitReady
// keeps New's real default (tests that fake the daemon set it to nil).
func newGuardManager(t *testing.T) *Manager {
	t.Helper()
	m := New()
	m.DataDir = t.TempDir()
	m.ReadyTimeout = 300 * time.Millisecond
	m.Kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	return m
}

func pidfileExists(t *testing.T, m *Manager) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(m.DataDir, "web.json"))
	return !os.IsNotExist(err)
}

// The daemon spawned by Start must actually serve before Start reports
// success: a child that exits immediately (the bind: address already in use
// case) is an error, not a silently written pidfile.
func TestStartFailsWhenDaemonExitsImmediately(t *testing.T) {
	m := newGuardManager(t)
	m.Spawn = func(argv []string, logFile string) (int, error) {
		cmd := exec.Command("true") // exits at once
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		_ = cmd.Wait() // reap so kill(pid,0) reports dead
		return cmd.Process.Pid, nil
	}
	// The real WaitReady: the process is dead, so it must report failure.
	if _, _, err := m.Start("127.0.0.1:3993"); err == nil {
		t.Fatal("start must fail when the daemon exits before serving")
	} else if !strings.Contains(err.Error(), "web.log") && !strings.Contains(err.Error(), "exited") {
		t.Fatalf("error should point at the exit/log: %v", err)
	}
	if pidfileExists(t, m) {
		t.Fatal("failed start must not leave a pidfile")
	}
}

// A stale pidfile plus an orphan woca process still holding the port is the
// restart trap: Start retires the orphan (validated by process name) and
// starts the fresh daemon cleanly.
func TestStartRetiresOrphanWocaOnPort(t *testing.T) {
	m := newGuardManager(t)

	orphan := exec.Command("sleep", "30")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = orphan.Wait(); close(reaped) }()
	m.PortOwner = func(addr string) (int, bool) { return orphan.Process.Pid, true }
	m.CommName = func(pid int) string { return "woca" }
	m.WaitReady = nil // the fake daemon serves nothing; skip the guard
	m.Spawn = newSleepSpawn(t)

	rec, _, err := m.Start("127.0.0.1:3994")
	if err != nil {
		t.Fatalf("start with orphan: %v", err)
	}
	if rec.PID == 0 || !pidfileExists(t, m) {
		t.Fatalf("record = %+v, pidfile missing", rec)
	}
	select {
	case <-reaped: // killed and reaped: genuinely gone
	case <-time.After(2 * time.Second):
		t.Fatal("orphan woca process should have been retired")
	}
	if _, err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}

// A non-woca process on the port is never killed; the failure names it so
// the user can act.
func TestStartDoesNotKillForeignPortOwner(t *testing.T) {
	m := newGuardManager(t)
	orphan := exec.Command("sleep", "30")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = orphan.Wait() }()
	m.PortOwner = func(addr string) (int, bool) { return orphan.Process.Pid, true }
	m.CommName = func(pid int) string { return "Chrome" }
	m.WaitReady = nil
	m.Spawn = func(argv []string, logFile string) (int, error) {
		t.Fatal("spawn must not run while a foreign process holds the port")
		return 0, nil
	}
	if _, _, err := m.Start("127.0.0.1:3995"); err == nil {
		t.Fatal("start must fail when a foreign process holds the port")
	} else if !strings.Contains(err.Error(), "Chrome") {
		t.Fatalf("error should name the holder: %v", err)
	}
	if !defaultAlive(orphan.Process.Pid) {
		t.Fatal("foreign process must not be killed")
	}
}

func newSleepSpawn(t *testing.T) func([]string, string) (int, error) {
	return func(argv []string, logFile string) (int, error) {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return cmd.Process.Pid, nil
	}
}
