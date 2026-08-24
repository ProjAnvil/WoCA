package bridgeproc

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// netListenLoopback binds an ephemeral loopback port (the fake bridge's
// bind step).
func netListenLoopback() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// fakeBridgeCommand builds a real serve process from the parent module —
// the same trick the main repo's tests use: re-exec the test binary in a
// special mode.
func fakeBridgeCommand(readyFile string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperBridgeProcess")
	cmd.Env = append(os.Environ(),
		"WOCA_GO_HELPER_BRIDGE=1",
		"WOCA_READY_FILE="+readyFile,
	)
	return cmd
}

// TestHelperBridgeProcess is not a real test: it is the fake bridge the
// manager spawns (writes the ready file, serves health, waits for exit).
func TestHelperBridgeProcess(t *testing.T) {
	if os.Getenv("WOCA_GO_HELPER_BRIDGE") == "" {
		return
	}
	ready := os.Getenv("WOCA_READY_FILE")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ln, err := netListenLoopback()
	if err != nil {
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String()
	_ = os.WriteFile(ready, []byte(url), 0o600)
	_ = (&http.Server{Handler: mux}).Serve(ln)
	os.Exit(0)
}

func TestManagerStartDiscoverStop(t *testing.T) {
	m := &Manager{Command: fakeBridgeCommand}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	baseURL, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !m.Running() {
		t.Fatal("not running after start")
	}
	// Second Start is idempotent and returns the same URL.
	again, err := m.Start(ctx)
	if err != nil || again != baseURL {
		t.Fatalf("restart = %q %v, want %q", again, err, baseURL)
	}
	// The health endpoint answers.
	resp, err := http.Get(baseURL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if m.Running() {
		t.Fatal("still running after stop")
	}
	if _, err := m.BaseURL(); err == nil {
		t.Fatal("base URL survived stop")
	}
}

func TestInstallVersionGuard(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(dir, "bundled-bridge")
	fake := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		fake = ""
	}
	if err := os.WriteFile(src, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	p1, err := Install(src, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != filepath.Join(home, ".woca", "native", "bin", "woca-bridge") {
		t.Fatalf("install path = %q", p1)
	}
	st, err := os.Stat(p1)
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("installed binary not executable: %v %v", st, err)
	}
	// Same version: no rewrite (mtime preserved).
	time.Sleep(10 * time.Millisecond)
	if _, err := Install(src, "v1"); err != nil {
		t.Fatal(err)
	}
	// Version drift: replaced.
	p2, err := Install(src, "v2")
	if err != nil || p2 != p1 {
		t.Fatalf("upgrade = %q %v", p2, err)
	}
	stamp, _ := os.ReadFile(filepath.Join(home, ".woca", "native", "bin", "woca-bridge.version"))
	if string(stamp) != "v2" {
		t.Fatalf("stamp = %q", stamp)
	}
}
