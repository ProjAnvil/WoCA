package webd

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// newTestManager builds a manager whose Spawn launches a real `sleep`
// process, rooted at a temp dir.
func newTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	m := New()
	m.DataDir = t.TempDir()
	var procs []*exec.Cmd
	reap := func() {
		for _, c := range procs {
			_ = c.Wait()
		}
	}
	t.Cleanup(reap)
	// The default Kill waits out its SIGTERM grace by polling kill(pid, 0),
	// which stays true for zombies until the test reaps them; SIGKILL keeps
	// the suite fast.
	m.Kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	m.Spawn = func(argv []string, logFile string) (int, error) {
		// Sanity: the harness always asks to run the serve subcommand with a
		// loopback addr and the no-browser flag.
		if argv[1] != "serve" || argv[2] != "--addr" || argv[len(argv)-1] != "--no-browser" {
			t.Fatalf("unexpected argv %v", argv)
		}
		cmd := exec.Command("sleep", "30")
		procs = append(procs, cmd)
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	}
	return m, reap
}

func TestStartStatusStopRestart(t *testing.T) {
	m, reap := newTestManager(t)

	// Stopping when not running is a no-op.
	if stopped, err := m.Stop(); err != nil || stopped {
		t.Fatalf("stop when idle = %v %v", stopped, err)
	}
	if rec := m.Status(); rec != nil {
		t.Fatalf("status when idle = %+v", rec)
	}

	rec, started, err := m.Start("127.0.0.1:3990")
	if err != nil || !started {
		t.Fatalf("start = %+v %v %v", rec, started, err)
	}
	if rec.Addr != "127.0.0.1:3990" || rec.PID == 0 {
		t.Fatalf("record = %+v", rec)
	}
	if got := m.Status(); got == nil || got.PID != rec.PID {
		t.Fatalf("status = %+v", got)
	}
	// The pidfile was written.
	data, err := os.ReadFile(filepath.Join(m.DataDir, "web.json"))
	if err != nil || len(data) == 0 {
		t.Fatalf("pidfile: %v", err)
	}

	// Second start is a no-op returning the same record.
	rec2, started2, err := m.Start("127.0.0.1:3990")
	if err != nil || started2 || rec2.PID != rec.PID {
		t.Fatalf("second start = %+v %v %v", rec2, started2, err)
	}

	stopped, err := m.Stop()
	if err != nil || !stopped {
		t.Fatalf("stop = %v %v", stopped, err)
	}
	// The sleep process is really gone (reap it first so it is not a
	// zombie, which would keep kill(pid, 0) succeeding).
	reap()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !defaultAlive(rec.PID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if defaultAlive(rec.PID) {
		t.Fatal("process still alive after stop")
	}
	if _, err := os.Stat(filepath.Join(m.DataDir, "web.json")); !os.IsNotExist(err) {
		t.Fatal("pidfile not removed")
	}

	// Restart works from both states.
	rec3, err := m.Restart("127.0.0.1:3991")
	if err != nil || rec3.PID == 0 || rec3.Addr != "127.0.0.1:3991" {
		t.Fatalf("restart = %+v %v", rec3, err)
	}
	if _, err := m.Restart(""); err != nil {
		t.Fatalf("restart while running: %v", err)
	}
	if _, err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestStalePidfileTreatedAsStopped(t *testing.T) {
	m, _ := newTestManager(t)
	// Write a pidfile for a process that cannot exist.
	if err := m.write(Record{PID: 1 << 30, Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if rec := m.Status(); rec != nil {
		t.Fatalf("stale record reported running: %+v", rec)
	}
	// Start overwrites the stale file.
	rec, started, err := m.Start("127.0.0.1:3992")
	if err != nil || !started {
		t.Fatalf("start over stale = %+v %v %v", rec, started, err)
	}
	if _, err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestParseAddr(t *testing.T) {
	cases := map[string]string{
		"":           DefaultAddrForTest,
		"3900":       "127.0.0.1:3900",
		"0.0.0.0:80": "0.0.0.0:80",
		" 3484 ":     "127.0.0.1:3484",
		"host:1234":  "host:1234",
	}
	for in, want := range cases {
		if got := ParseAddr(in); got != want {
			t.Errorf("ParseAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

const DefaultAddrForTest = "127.0.0.1:3484"
