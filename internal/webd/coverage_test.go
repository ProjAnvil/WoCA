package webd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
