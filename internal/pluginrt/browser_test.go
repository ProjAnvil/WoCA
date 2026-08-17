package pluginrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// helperMain is a stand-in pinchtab server used to exercise the spawn/health
// protocol with a real subprocess. It reads the isolated config pointed at by
// PINCHTAB_CONFIG, waits for an optional trigger file, then serves the API.
const helperMain = `package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	switch os.Getenv("WOCA_TEST_HELPER_MODE") {
	case "exit":
		fmt.Fprintln(os.Stderr, "helper: simulated startup crash")
		os.Exit(3)
	case "hang":
		time.Sleep(24 * time.Hour)
	}
	raw, err := os.ReadFile(os.Getenv("PINCHTAB_CONFIG"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: read config:", err)
		os.Exit(2)
	}
	var cfg struct {
		Server struct {
			Port  string
			Bind  string
			Token string
		}
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "helper: parse config:", err)
		os.Exit(2)
	}
	if wait := os.Getenv("WOCA_TEST_HELPER_WAIT_FILE"); wait != "" {
		for {
			if _, err := os.Stat(wait); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/navigate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tabId": "helper-tab", "title": "Helper Page", "url": "https://helper.test/"})
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://helper.test/", "title": "Helper Page", "text": "helper body text", "textLength": 16})
	})
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "[e9] button \"Go\"")
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{\"ok\":true}")
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Server.Bind, cfg.Server.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: listen:", err)
		os.Exit(4)
	}
	fmt.Fprintf(os.Stderr, "helper: listening on %s\n", cfg.Server.Port)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintln(os.Stderr, "helper: serve:", err)
		os.Exit(5)
	}
}
`

var (
	helperOnce sync.Once
	helperDir  string
	helperBin  string
	helperErr  error
)

// pinchtabHelper builds (once) and returns the helper binary path. The build
// runs with proxies stripped so it can never reach the network.
func pinchtabHelper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		var err error
		helperDir, err = os.MkdirTemp("", "pluginrt-pinchtab-helper-*")
		if err != nil {
			helperErr = err
			return
		}
		src := filepath.Join(helperDir, "main.go")
		if err := os.WriteFile(src, []byte(helperMain), 0o644); err != nil {
			helperErr = err
			return
		}
		bin := filepath.Join(helperDir, "pinchtab-helper")
		cmd := exec.Command("go", "build", "-o", bin, src)
		cmd.Env = append(os.Environ(),
			"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			helperErr = fmt.Errorf("build helper: %v: %s", err, out)
			return
		}
		helperBin = bin
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	return helperBin
}

// reservePort picks a currently free loopback port (tiny TOCTOU window, but
// tests are sequential).
func reservePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// writeBrowserConfig writes a complete isolated config so loadConfig does not
// rewrite it; the runtime then talks to whatever listens on port.
func writeBrowserConfig(t *testing.T, root, port, token string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"server":           map[string]any{"port": port, "bind": "127.0.0.1", "token": token},
		"instanceDefaults": map[string]any{"mode": "headless"},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "config.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// installHelperBinary copies the built helper into the runtime's binary slot.
func installHelperBinary(t *testing.T, b *BrowserRuntime) {
	t.Helper()
	dest := b.binaryPath()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pinchtabHelper(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, raw, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserClientAdoptsRunningServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)

	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, port, "adopted-token")
	// The binary only needs to exist for the Installed gate; it is never run.
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	c, err := b.Client(context.Background())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	nav, err := c.navigate(context.Background(), "https://example.com")
	if err != nil || nav.Title != "" {
		t.Fatalf("navigate via adopted server: %+v %v", nav, err)
	}
	// Second call takes the fast path with the identical client.
	c2, err := b.Client(context.Background())
	if err != nil || c2 != c {
		t.Fatalf("fast path: %v (client changed: %v)", err, c2 != c)
	}
}

func TestBrowserCallRestartsDeadServer(t *testing.T) {
	var phase int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Connection: close keeps the client from pooling keep-alive
		// connections, so the helper can rebind the port after Close.
		w.Header().Set("Connection", "close")
		if atomic.LoadInt32(&phase) == 0 && r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Error(w, "server winding down", http.StatusInternalServerError)
	}))
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)

	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	defer b.Stop()
	writeBrowserConfig(t, root, port, "tok")
	installHelperBinary(t, b)

	ctx := context.Background()
	// First call adopts the fake server; force a transport failure by
	// tearing it down, then the retry must spawn the helper binary.
	var calls int32
	err := b.call(ctx, func(c *pinchClient) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			if err := c.health(ctx); err != nil {
				t.Fatalf("initial health: %v", err)
			}
			atomic.StoreInt32(&phase, 1)
			srv.Close()
			// Give the closed listener a beat to release the port.
			time.Sleep(100 * time.Millisecond)
			_, err := c.navigate(ctx, "https://example.com")
			return err // transport error against the dead server
		}
		nav, err := c.navigate(ctx, "https://example.com")
		if err != nil {
			return err
		}
		if nav.Title != "Helper Page" {
			t.Fatalf("retry served by wrong peer: %+v", nav)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("call with restart: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fn calls = %d, want 2", got)
	}
}

func TestBrowserSpawnFailureExitsDuringStartup(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, reservePort(t), "tok")
	// A binary that dies instantly: spawn must surface the exit plus the
	// server-log tail.
	script := "#!/bin/sh\necho 'pinchtab: boom' >&2\nexit 3\n"
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := b.Client(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("err = %v, want exited during startup", err)
	}
	if !strings.Contains(err.Error(), "pinchtab: boom") {
		t.Fatalf("log tail missing from error: %v", err)
	}
}

func TestBrowserSpawnContextCanceled(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, reservePort(t), "tok")
	// Never-healthy binary that stays alive: only the ctx cancellation ends
	// the spawn loop.
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	// exec keeps the kill on target: without it the shell's child sleep
	// would survive cmd.Process.Kill as an orphan.
	if err := os.WriteFile(b.binaryPath(), []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := b.Client(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("cancel took %s", time.Since(start))
	}
}

func TestBrowserStopRacesSpawn(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, reservePort(t), "tok")
	installHelperBinary(t, b)

	// The helper refuses to become healthy until the trigger file exists, so
	// Stop() deterministically lands while the spawn is still in flight.
	trigger := filepath.Join(t.TempDir(), "go")
	t.Setenv("WOCA_TEST_HELPER_WAIT_FILE", trigger)

	res := make(chan error, 1)
	go func() {
		_, err := b.Client(context.Background())
		res <- err
	}()
	// Wait until the spawn is actually underway (it creates the server log),
	// so Stop lands between the slow path's stopped-check and setClient.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(b.serverLogPath()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spawn never started (no server log)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop() // happens-before the trigger below => setClient must refuse
	if err := os.WriteFile(trigger, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "stopped during startup") {
			t.Fatalf("err = %v, want stopped during startup", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Client did not return after the spawn was discarded")
	}
}

func TestBrowserStopKillsSpawnedServer(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, reservePort(t), "tok")
	installHelperBinary(t, b)

	c, err := b.Client(context.Background())
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	proc := b.proc
	if proc == nil {
		t.Fatal("spawned process not recorded")
	}
	b.Stop()
	if err := c.health(context.Background()); err == nil {
		t.Fatal("server must be dead after Stop")
	}
	if _, err := b.Client(context.Background()); !errors.Is(err, ErrBrowserStopped) {
		t.Fatalf("post-stop client: %v", err)
	}
}

func TestBrowserInstallPaths(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())

	// Already present: install is a no-op with a progress line.
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := b.Install(context.Background(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("idempotent install: %v", err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "already present") {
		t.Fatalf("progress = %v", lines)
	}

	// Missing binary: the pinned GitHub download is refused by the dead
	// proxy pinned in TestMain, so the failure path runs without network.
	fresh := NewBrowserRuntime(t.TempDir(), zerolog.Nop())
	err := fresh.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "download pinchtab") {
		t.Fatalf("err = %v, want download pinchtab failure", err)
	}
}

func TestLoadConfigErrorPaths(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Root below a regular file: MkdirAll fails while persisting.
	b := NewBrowserRuntime(filepath.Join(blocker, "pt"), zerolog.Nop())
	if _, _, err := b.loadConfig(); err == nil {
		t.Fatal("config under a file must fail")
	}
	// config.json occupied by a directory: WriteFile fails.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	b2 := NewBrowserRuntime(root, zerolog.Nop())
	if _, _, err := b2.loadConfig(); err == nil {
		t.Fatal("config.json as dir must fail")
	}
}

func TestBrowserServerLogPath(t *testing.T) {
	b := NewBrowserRuntime(t.TempDir(), zerolog.Nop())
	if got := b.serverLogPath(); !strings.HasSuffix(got, "server.log") {
		t.Fatalf("serverLogPath = %q", got)
	}
	// Missing log degrades to a placeholder instead of an error.
	if got := b.logTail(); got != "(no server log)" {
		t.Fatalf("logTail without log = %q", got)
	}
}

func TestPinchErrorPayloads(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"code":"err_code","error":"something broke"}`, "err_code: something broke"},
		{`{"error":"just a message"}`, "just a message"},
		{`{"code":"orphan"}`, `{"code":"orphan"}`}, // no error field: raw passthrough
		{`not json at all`, "not json at all"},
		{`  {"error":"trimmed"}  `, "trimmed"}, // surrounding whitespace still parses
	}
	for _, c := range cases {
		if got := pinchError([]byte(c.raw)); got != c.want {
			t.Errorf("pinchError(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestPinchClientDoErrorPaths(t *testing.T) {
	ctx := context.Background()

	// Malformed base URL: request construction fails.
	bad := &pinchClient{base: "http://bad\x7fhost", token: "t"}
	if err := bad.do(ctx, http.MethodGet, "/health", nil, nil); err == nil {
		t.Fatal("invalid URL must fail")
	}

	// Unmarshalable body: JSON encoding fails before the request.
	ok := &pinchClient{base: "http://127.0.0.1:1", token: "t"}
	if err := ok.do(ctx, http.MethodPost, "/action", map[string]any{"f": func() {}}, nil); err == nil {
		t.Fatal("unmarshalable body must fail")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/teapot":
			http.Error(w, `{"code":"teapot","error":"short and stout"}`, http.StatusTeapot)
		case "/aborted":
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write([]byte("cut"))
			panic(http.ErrAbortHandler)
		}
	}))
	defer srv.Close()
	c := &pinchClient{base: srv.URL, token: "t"}

	// HTTP >= 400 with a JSON error payload.
	err := c.do(ctx, http.MethodGet, "/teapot", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "teapot: short and stout") {
		t.Fatalf("teapot: %v", err)
	}
	// Body truncated mid-read.
	if err := c.do(ctx, http.MethodGet, "/aborted", nil, nil); err == nil {
		t.Fatal("aborted body must fail")
	}
	// 200 with a body that is not the expected JSON.
	var out struct{ Status string }
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<<not json>>"))
	}))
	defer srv2.Close()
	c2 := &pinchClient{base: srv2.URL, token: "t"}
	if err := c2.do(ctx, http.MethodGet, "/health", nil, &out); err == nil {
		t.Fatal("invalid JSON response must fail")
	}
	// Navigate failure without a code, and text with default/raw query flags.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/navigate" {
			_, _ = w.Write([]byte(`{"error":"denied"}`))
			return
		}
		_, _ = w.Write([]byte(`{"url":"u","title":"t","text":"x","textLength":1}`))
	}))
	defer srv3.Close()
	c3 := &pinchClient{base: srv3.URL, token: "t"}
	if _, err := c3.navigate(ctx, "https://example.com"); err == nil || !strings.Contains(err.Error(), "navigate: denied") {
		t.Fatalf("codeless navigate error: %v", err)
	}
	res, err := c3.text(ctx, 0, true)
	if err != nil || res.Text != "x" {
		t.Fatalf("text(0, raw): %+v %v", res, err)
	}
}

func TestBrowserInstallUnsupportedPlatform(t *testing.T) {
	pinPlatform(t, "plan9", "mips")
	b := NewBrowserRuntime(t.TempDir(), zerolog.Nop())
	err := b.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported platform plan9-mips for pinchtab") {
		t.Fatalf("err = %v", err)
	}
}

func TestBrowserClientConcurrentAdoption(t *testing.T) {
	// The health probe stalls so a second caller waits on startMu and then
	// takes the double-checked fast path with the first caller's client.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			<-release
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port), "tok")
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	first := make(chan *pinchClient, 1)
	go func() {
		c, err := b.Client(context.Background())
		if err != nil {
			t.Error(err)
		}
		first <- c
	}()
	// Wait until the first caller is inside the (blocked) health probe.
	time.Sleep(150 * time.Millisecond)
	second := make(chan *pinchClient, 1)
	go func() {
		c, err := b.Client(context.Background())
		if err != nil {
			t.Error(err)
		}
		second <- c
	}()
	time.Sleep(150 * time.Millisecond)
	close(release)

	c1, c2 := <-first, <-second
	if c1 == nil || c1 != c2 {
		t.Fatalf("concurrent clients differ: %p vs %p", c1, c2)
	}
}

func TestBrowserClientConfigUnwritable(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	// Binary present but config.json blocked by a directory: the loadConfig
	// failure surfaces from Client.
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Client(context.Background()); err == nil {
		t.Fatal("unwritable config must fail Client")
	}
}

func TestBrowserCallRestartFails(t *testing.T) {
	// Adopt a server, kill it, and leave a garbage binary behind: the retry
	// must fail cleanly when the respawn cannot even start.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	defer b.Stop()
	writeBrowserConfig(t, root, strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port), "tok")
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("definitely not an executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := b.Client(ctx); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	srv.Close()
	var attempts int32
	err := b.call(ctx, func(c *pinchClient) error {
		atomic.AddInt32(&attempts, 1)
		_, err := c.navigate(ctx, "https://example.com")
		return err
	})
	// The transport failure triggers exactly one restart attempt, whose spawn
	// failure must surface (no retry against an unstartable binary).
	if err == nil || !strings.Contains(err.Error(), "start pinchtab") || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("err = %v, attempts = %d (want failed restart after one attempt)", err, attempts)
	}
}

func TestBrowserSpawnCannotOpenLog(t *testing.T) {
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	writeBrowserConfig(t, root, reservePort(t), "tok")
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A read-only root keeps spawn from creating the server log.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	_, err := b.Client(context.Background())
	if err == nil {
		t.Fatal("spawn without a writable log must fail")
	}
}

// resetConn hijacks the connection, writes a partial body against a bogus
// Content-Length, and closes with SO_LINGER 0 so the client sees a reset
// mid-read instead of a clean EOF.
func resetConn(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 4096\r\nContent-Type: application/json\r\n\r\npartial")
	_ = buf.Flush()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

func TestPinchClientReadBodyReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resetConn(w)
	}))
	defer srv.Close()
	c := &pinchClient{base: srv.URL, token: "t"}
	var payload struct{ Status string }
	if err := c.do(context.Background(), http.MethodGet, "/health", nil, &payload); err == nil {
		t.Fatal("reset mid-body must fail the read")
	}
}
