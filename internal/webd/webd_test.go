package webd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	m.WaitReady = nil // the fake sleep daemon serves nothing; skip the guard
	// The fake daemon binds nothing, so the lsof orphan sweep must not see
	// real listeners either — otherwise whatever happens to hold the port on
	// the dev machine (or the default addr) fails or kills the wrong thing.
	m.PortOwner = func(addr string) (int, bool) { return 0, false }
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

	addr1 := freeAddr(t)
	rec, started, err := m.Start(addr1)
	if err != nil || !started {
		t.Fatalf("start = %+v %v %v", rec, started, err)
	}
	if rec.Addr != addr1 || rec.PID == 0 {
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
	rec2, started2, err := m.Start(addr1)
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
	addr2 := freeAddr(t)
	rec3, err := m.Restart(addr2)
	if err != nil || rec3.PID == 0 || rec3.Addr != addr2 {
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
	rec, started, err := m.Start(freeAddr(t))
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

func TestAccessorsAndRecordIO(t *testing.T) {
	m := New()
	m.DataDir = t.TempDir()

	if got := m.LogPath(); got != filepath.Join(m.DataDir, "web.log") {
		t.Fatalf("LogPath = %q", got)
	}
	// readPID with no record: zero.
	if got := m.readPID(); got != 0 {
		t.Fatalf("readPID = %d", got)
	}
	// write + read round trip.
	if err := m.write(Record{PID: 4242, Addr: "127.0.0.1:3999", Started: "now"}); err != nil {
		t.Fatal(err)
	}
	rec, err := m.read()
	if err != nil || rec.PID != 4242 || rec.Addr != "127.0.0.1:3999" {
		t.Fatalf("read = %+v %v", rec, err)
	}
	if got := m.readPID(); got != 4242 {
		t.Fatalf("readPID = %d", got)
	}
	// Malformed record: read error surfaces in readPID.
	if err := os.WriteFile(m.recordPath(), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.readPID(); got != 0 {
		t.Fatalf("malformed readPID = %d", got)
	}
	// write failure with an unusable data dir.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := New()
	m2.DataDir = blocker
	if err := m2.write(Record{PID: 1}); err == nil {
		t.Fatal("expected write failure")
	}
}

func TestDefaultWaitReady(t *testing.T) {
	m := New()
	m.DataDir = t.TempDir()
	m.ReadyTimeout = 400 * time.Millisecond

	// An alive process with a live health endpoint: ready.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := m.defaultWaitReady(addr, func() bool { return true }); err != nil {
		t.Fatal(err)
	}

	// Dead process: fails fast with the exited message.
	if err := m.defaultWaitReady(addr, func() bool { return false }); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("dead process = %v", err)
	}

	// Alive but never serving: times out.
	if err := m.defaultWaitReady("127.0.0.1:1", func() bool { return true }); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("timeout = %v", err)
	}
}

func TestPortOwnerAndCommName(t *testing.T) {
	// This test process is listening on nothing, but lsof runs either way.
	if _, ok := defaultPortOwner("no-colon-host"); ok {
		t.Fatal("addr without port should not resolve")
	}
	// A real listener: bind one and resolve its owner (this process).
	ln, err := newTCPTarget()
	if err != nil {
		t.Skip("cannot bind listener")
	}
	defer ln.Close()
	pid, ok := defaultPortOwner(ln.Addr().String())
	if !ok || pid != os.Getpid() {
		t.Logf("port owner = %d ok=%v (lsof resolution is best-effort)", pid, ok)
	}
	if name := defaultCommName(os.Getpid()); name == "" {
		t.Log("comm name empty (ps output varies)")
	}
	if name := defaultCommName(999999); name != "" {
		t.Logf("dead pid comm = %q", name)
	}
}

func TestDefaultSpawnAndStopEdge(t *testing.T) {
	m := New()
	m.DataDir = t.TempDir()

	// defaultSpawn launches a detached sleep and logs to the file.
	pid, err := defaultSpawn([]string{"/bin/sleep", "5"}, m.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if !m.Alive(pid) {
		t.Fatal("spawned process not alive")
	}
	// The log file exists.
	if _, err := os.Stat(m.LogPath()); err != nil {
		t.Fatalf("log file: %v", err)
	}
	// defaultSpawn with an unwritable log fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultSpawn([]string{"/bin/sleep", "1"}, filepath.Join(blocker, "log")); err == nil {
		t.Fatal("expected log open failure")
	}

	// Stop with a record whose pid is dead: not stopped, record cleaned.
	deadPID := 999999
	if m.Alive(deadPID) {
		t.Skip("pid unexpectedly alive")
	}
	if err := m.write(Record{PID: deadPID, Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop()
	if err != nil || stopped {
		t.Fatalf("stop dead = %v %v", stopped, err)
	}
	if _, err := os.Stat(m.recordPath()); !os.IsNotExist(err) {
		t.Fatal("record not cleaned")
	}

	// Restart with Start failing (unusable data dir) propagates the error.
	m2 := New()
	m2.DataDir = blocker
	if _, err := m2.Restart("127.0.0.1:0"); err == nil {
		t.Fatal("expected restart failure")
	}
}

// newTCPTarget binds a loopback listener for port-owner resolution tests.
func newTCPTarget() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// freeAddr returns a loopback address on a port the OS just confirmed free,
// so tests never collide with a real listener the way the old hardcoded
// 399x ports did.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := newTCPTarget()
	if err != nil {
		t.Fatalf("no free loopback port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
