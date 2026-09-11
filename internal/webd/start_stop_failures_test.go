package webd

// Gap tests: constructor fallbacks, injected spawn/kill failures, the
// default process helpers, and restart-over-failing-stop.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNewFallsBackToTempDirWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	m := New()
	if m.DataDir == "" {
		t.Fatal("DataDir should fall back to the temp dir")
	}
}

func TestWriteRecordFailures(t *testing.T) {
	m := New()
	m.DataDir = t.TempDir()
	if err := m.write(Record{PID: 1}); err != nil {
		t.Fatal(err)
	}
	// A directory occupying the record path makes the write fail.
	m2 := New()
	m2.DataDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(m2.DataDir, "web.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m2.write(Record{PID: 1}); err == nil {
		t.Fatal("write onto a directory should fail")
	}
}

// newFakeManager wires injectable process hooks with no port owner.
func newFakeManager(t *testing.T) *Manager {
	t.Helper()
	m := New()
	m.DataDir = t.TempDir()
	m.WaitReady = nil
	m.Spawn = func(argv []string, log string) (int, error) { return 4242, nil }
	m.Alive = func(pid int) bool { return pid == 4242 }
	m.Kill = func(pid int) error { return nil }
	return m
}

func TestStartWithoutPortOwnerSkipsSweep(t *testing.T) {
	m := newFakeManager(t)
	m.PortOwner = nil
	if m.PortOwner != nil {
		t.Fatal("test precondition: no port owner")
	}
	rec, started, err := m.Start("127.0.0.1:0")
	if err != nil || !started || rec.PID != 4242 {
		t.Fatalf("start = %+v %v %v", rec, started, err)
	}
}

func TestStartSpawnFailure(t *testing.T) {
	m := newFakeManager(t)
	boom := errors.New("spawn refused")
	m.Spawn = func(argv []string, log string) (int, error) { return 0, boom }
	if _, _, err := m.Start("127.0.0.1:0"); err == nil {
		t.Fatal("spawn failure should surface")
	}
}

func TestStartRecordWriteFailure(t *testing.T) {
	m := newFakeManager(t)
	if err := os.MkdirAll(filepath.Join(m.DataDir, "web.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Start("127.0.0.1:0"); err == nil {
		t.Fatal("record write failure should surface")
	}
}

func TestSweepOrphanBranches(t *testing.T) {
	t.Run("recorded daemon is left to Stop", func(t *testing.T) {
		m := newFakeManager(t)
		if err := m.write(Record{PID: 4242, Addr: "a"}); err != nil {
			t.Fatal(err)
		}
		m.PortOwner = func(addr string) (int, bool) { return 4242, true }
		if err := m.sweepOrphan("a"); err != nil {
			t.Fatalf("recorded daemon should not be swept: %v", err)
		}
	})
	t.Run("kill failure surfaces", func(t *testing.T) {
		m := newFakeManager(t)
		m.PortOwner = func(addr string) (int, bool) { return 9999, true }
		m.CommName = func(pid int) string { return "woca-serve" }
		boom := errors.New("kill refused")
		m.Kill = func(pid int) error { return boom }
		if err := m.sweepOrphan("a"); err == nil {
			t.Fatal("orphan kill failure should surface")
		}
	})
	t.Run("no owner is a no-op", func(t *testing.T) {
		m := newFakeManager(t)
		m.PortOwner = func(addr string) (int, bool) { return 0, false }
		if err := m.sweepOrphan("a"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStopAndRestartKillFailures(t *testing.T) {
	m := newFakeManager(t)
	if err := m.write(Record{PID: 4242, Addr: "a"}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("kill refused")
	m.Kill = func(pid int) error { return boom }
	if _, err := m.Stop(); err == nil {
		t.Fatal("Stop kill failure should surface")
	}
	if _, err := m.Restart("a"); err == nil {
		t.Fatal("Restart over failing Stop should surface")
	}
}

func TestDefaultProcessHelpers(t *testing.T) {
	// defaultSpawn with a nonexistent binary errors.
	if _, err := defaultSpawn([]string{"/nonexistent-binary-xyz"}, filepath.Join(t.TempDir(), "log")); err == nil {
		t.Fatal("defaultSpawn of a missing binary should fail")
	}

	// defaultAlive/defaultKill over a real child process.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !defaultAlive(pid) {
		t.Fatal("fresh child should be alive")
	}
	if err := defaultKill(pid); err != nil {
		t.Fatalf("defaultKill: %v", err)
	}
	// Reap the child concurrently so the liveness probe sees a real exit,
	// not an unreaped zombie.
	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit after SIGTERM")
	}
	if defaultAlive(pid) {
		t.Fatal("reaped child should not be alive")
	}
}
