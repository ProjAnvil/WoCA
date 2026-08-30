package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestWebDaemonLifecycle(t *testing.T) {
	isolateEnv(t)
	addr := freeLocalAddr(t)

	if err := execCmd(t, "web", "start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "web", "status"); err != nil {
		t.Fatal(err)
	}
	// A second start reports the already-running daemon.
	if err := execCmd(t, "web", "start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	addr2 := freeLocalAddr(t)
	if err := execCmd(t, "web", "restart", "--addr", addr2); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "web", "stop"); err != nil {
		t.Fatal(err)
	}
}

func TestWebStatusAndStopWithRecords(t *testing.T) {
	home := isolateEnv(t)
	dataDir := filepath.Join(home, ".woca")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRecord := func(pid int, addr string) {
		t.Helper()
		rec := map[string]any{"pid": pid, "addr": addr, "started": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dataDir, "web.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A record whose pid is this test process: status reports running.
	writeRecord(os.Getpid(), "127.0.0.1:3998")
	if err := execCmd(t, "web", "status"); err != nil {
		t.Fatal(err)
	}

	// A record pointing at a spawned sleep process: stop kills it.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleep.Process.Kill(); _ = sleep.Wait() }()
	writeRecord(sleep.Process.Pid, "127.0.0.1:3998")
	if err := execCmd(t, "web", "stop"); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenAICustomToolFlow streams one tool call for the given tool/args on
// the first request and plain text afterwards.

func TestRunREPLWebGUI(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "web-ok")
	opts := baseOpts(t, url)

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/web 127.0.0.1:0\n/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "web gui started") {
		t.Fatalf("web start output = %q", combined)
	}
	m := regexp.MustCompile(`web gui started: http://(127\.0\.0\.1:\d+)`).FindStringSubmatch(combined)
	if m == nil {
		t.Fatal("no address in web output")
	}
	base := "http://" + m[1]

	call := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// The webgui server shares the closures from newConsole: quota,
	// thinking, settings rebuilds, claude-env, plugins.
	if code := call(http.MethodGet, "/api/quota", ""); code != http.StatusOK {
		t.Fatalf("quota = %d", code)
	}
	if code := call(http.MethodPost, "/api/agent/thinking", `{"budget":0}`); code != http.StatusOK {
		t.Fatalf("thinking = %d", code)
	}
	if code := call(http.MethodPut, "/api/settings", `{"provider":"openai","memory_engine":"none"}`); code != http.StatusOK {
		t.Fatalf("settings = %d", code)
	}
	if code := call(http.MethodPut, "/api/claude-env", `{"skills":[],"mcp":[]}`); code != http.StatusOK {
		t.Fatalf("claudeenv = %d", code)
	}
	if code := call(http.MethodGet, "/api/plugins", ""); code != http.StatusOK {
		t.Fatalf("plugins = %d", code)
	}
	// A real builtin plugin name passes validation; the (offline) install
	// fails inside the plugin manager, executing the closure.
	call(http.MethodPost, "/api/plugins/browser/install", "{}")
	call(http.MethodPost, "/api/plugins/browser/reinstall", "{}")
}
