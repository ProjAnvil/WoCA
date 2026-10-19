package cfbridge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
)

// --- fake Cloudflare sandbox bridge -----------------------------------------

type execCall struct {
	Argv      []string
	Cwd       string
	TimeoutMs int64
	Body      map[string]json.RawMessage
}

// sseResp lets test handlers hand-write SSE event frames.
type sseResp struct {
	w   http.ResponseWriter
	ctx context.Context // request context, for handlers that hold streams open
}

func (s sseResp) event(name, data string) {
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s sseResp) stdout(data string) { s.event("stdout", b64(data)) }
func (s sseResp) stderr(data string) { s.event("stderr", b64(data)) }
func (s sseResp) exit(code int)      { s.event("exit", fmt.Sprintf(`{"exit_code":%d}`, code)) }

// fakeBridge serves the CF bridge API shape from memory. execFn programs
// the SSE responses; everything else is bookkeeping the tests assert on.
type fakeBridge struct {
	mu         sync.Mutex
	addr       string
	sbxID      string
	running    bool
	execFn     func(call execCall, w sseResp)
	execCT     string // exec response content-type (default text/event-stream)
	execStatus int    // non-zero: respond with this status instead of streaming
	execBody   string

	created     int
	destroyed   int
	authSeen    []string
	calls       []execCall
	files       map[string][]byte
	filePuts    []string // decoded put paths, no leading slash
	putCT       string
	fileGetPath string // escaped path of the last GET
	hydrates    [][]byte
	hydrateCT   string
	persistQ    string
	persistCnt  int
}

func newFake(t *testing.T) *fakeBridge {
	t.Helper()
	fb := &fakeBridge{
		sbxID:   "sbx_test",
		running: true,
		files:   map[string][]byte{},
		execCT:  "text/event-stream",
		execFn: func(call execCall, w sseResp) {
			w.exit(0)
		},
	}
	srv := httptest.NewServer(fb.handler())
	t.Cleanup(srv.Close)
	fb.addr = srv.URL
	return fb
}

func newBridge(t *testing.T, fb *fakeBridge) *Bridge {
	t.Helper()
	b, err := New(Config{Endpoint: fb.addr, APIKey: "k1", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("cfbridge.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func (fb *fakeBridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandbox", fb.handleCreate)
	mux.HandleFunc("DELETE /v1/sandbox/{id}", fb.handleDestroy)
	mux.HandleFunc("GET /v1/sandbox/{id}/running", fb.handleRunning)
	mux.HandleFunc("POST /v1/sandbox/{id}/exec", fb.handleExec)
	mux.HandleFunc("GET /v1/sandbox/{id}/file/{path...}", fb.handleFileGet)
	mux.HandleFunc("PUT /v1/sandbox/{id}/file/{path...}", fb.handleFilePut)
	mux.HandleFunc("POST /v1/sandbox/{id}/persist", fb.handlePersist)
	mux.HandleFunc("POST /v1/sandbox/{id}/hydrate", fb.handleHydrate)
	return mux
}

func (fb *fakeBridge) record(r *http.Request) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.authSeen = append(fb.authSeen, r.Header.Get("Authorization"))
}

func (fb *fakeBridge) checkID(r *http.Request) bool {
	return r.PathValue("id") == fb.sbxID
}

func (fb *fakeBridge) handleCreate(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	fb.mu.Lock()
	fb.created++
	fb.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": fb.sbxID})
}

func (fb *fakeBridge) handleDestroy(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	fb.mu.Lock()
	fb.destroyed++
	fb.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (fb *fakeBridge) handleRunning(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	fb.mu.Lock()
	running := fb.running
	fb.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"running": running})
}

func (fb *fakeBridge) handleExec(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	var raw []byte
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	call := execCall{Body: map[string]json.RawMessage{}}
	if err := json.Unmarshal(raw, &call.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.Unmarshal(call.Body["argv"], &call.Argv)
	_ = json.Unmarshal(call.Body["cwd"], &call.Cwd)
	_ = json.Unmarshal(call.Body["timeout_ms"], &call.TimeoutMs)
	fb.mu.Lock()
	fb.calls = append(fb.calls, call)
	status, statusBody, ct := fb.execStatus, fb.execBody, fb.execCT
	fn := fb.execFn
	fb.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(statusBody))
		return
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	fn(call, sseResp{w: w, ctx: r.Context()})
}

func (fb *fakeBridge) handleFileGet(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	fb.mu.Lock()
	fb.fileGetPath = r.URL.EscapedPath()
	data, ok := fb.files[r.PathValue("path")]
	fb.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"file not found"}`))
		return
	}
	_, _ = w.Write(data)
}

func (fb *fakeBridge) handleFilePut(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fb.mu.Lock()
	fb.files[r.PathValue("path")] = data
	fb.filePuts = append(fb.filePuts, r.PathValue("path"))
	fb.putCT = r.Header.Get("Content-Type")
	fb.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (fb *fakeBridge) handlePersist(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	fb.mu.Lock()
	fb.persistQ = r.URL.RawQuery
	fb.persistCnt++
	var body []byte
	if n := len(fb.hydrates); n > 0 {
		body = fb.hydrates[n-1]
	} else {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.Close()
		body = buf.Bytes()
	}
	fb.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-tar")
	_, _ = w.Write(body)
}

func (fb *fakeBridge) handleHydrate(w http.ResponseWriter, r *http.Request) {
	fb.record(r)
	if !fb.checkID(r) {
		http.NotFound(w, r)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fb.mu.Lock()
	fb.hydrates = append(fb.hydrates, data)
	fb.hydrateCT = r.Header.Get("Content-Type")
	fb.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// --- fake assertions ---------------------------------------------------------

func (fb *fakeBridge) lastExec(t *testing.T) execCall {
	t.Helper()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.calls) == 0 {
		t.Fatal("no exec calls recorded")
	}
	return fb.calls[len(fb.calls)-1]
}

func (fb *fakeBridge) requireAuth(t *testing.T, want string) {
	t.Helper()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.authSeen) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, a := range fb.authSeen {
		if a != want {
			t.Fatalf("Authorization = %q, want %q", a, want)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// --- SSE parser unit tests ---------------------------------------------------

func TestScanSSE(t *testing.T) {
	stream := "event: stdout\ndata: aGk=\n\n" +
		": keepalive comment\n\n" +
		"event:stderr\ndata:dHdv\r\n\n" + // no space after colon, CRLF line
		"event: multi\ndata: line1\ndata: line2\n\n" +
		"event: exit\ndata: {\"exit_code\":0}" // unterminated tail block
	var got []string
	err := scanSSE(strings.NewReader(stream), func(name, data string) bool {
		got = append(got, name+"|"+data)
		return true
	})
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	want := []string{
		"stdout|aGk=",
		"stderr|dHdv",
		"multi|line1\nline2",
		"exit|{\"exit_code\":0}",
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanSSEStopEarly(t *testing.T) {
	stream := "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"
	n := 0
	err := scanSSE(strings.NewReader(stream), func(name, data string) bool {
		n++
		return false
	})
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	if n != 1 {
		t.Fatalf("callback fired %d times, want 1", n)
	}
}

func TestScanSSEEmpty(t *testing.T) {
	err := scanSSE(strings.NewReader(""), func(name, data string) bool { t.Fatal("no events expected"); return true })
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
}

// --- lifecycle ---------------------------------------------------------------

func TestNewCreatesSandboxAndCloseDestroys(t *testing.T) {
	fb := newFake(t)
	b, err := New(Config{Endpoint: fb.addr, APIKey: "k1", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.sbxID != "sbx_test" {
		t.Fatalf("sbxID = %q, want sbx_test", b.sbxID)
	}
	fb.mu.Lock()
	created := fb.created
	fb.mu.Unlock()
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	fb.requireAuth(t, "Bearer k1")

	ok, err := b.Running(t.Context())
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !ok {
		t.Fatal("Running = false, want true")
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fb.mu.Lock()
	destroyed := fb.destroyed
	fb.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("destroyed = %d, want 1", destroyed)
	}
	// Close is idempotent: a second call must not issue another DELETE.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	fb.mu.Lock()
	destroyed = fb.destroyed
	fb.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("destroyed after second Close = %d, want 1", destroyed)
	}
}

func TestRunningFalse(t *testing.T) {
	fb := newFake(t)
	fb.mu.Lock()
	fb.running = false
	fb.mu.Unlock()
	b := newBridge(t, fb)
	ok, err := b.Running(t.Context())
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if ok {
		t.Fatal("Running = true, want false")
	}
}

func TestNewInvalidEndpoint(t *testing.T) {
	for _, ep := range []string{"", "localhost:8080", "http://"} {
		_, err := New(Config{Endpoint: ep})
		if err == nil {
			t.Fatalf("New(endpoint=%q): want error, got nil", ep)
		}
	}
}

// --- Exec --------------------------------------------------------------------

func TestExecAggregatesSSE(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("hi ")
		w.stdout("there")
		w.stderr("warn")
		w.exit(0)
	}
	b := newBridge(t, fb)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, err := b.Exec(ctx, executor.ExecRequest{
		Name:  "echo",
		Args:  []string{"x"},
		Dir:   "/workspace",
		Env:   []string{"K=V"}, // not supported by the bridge API
		Stdin: []byte("s"),     // not supported by the bridge API
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "hi there" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hi there")
	}
	if string(res.Stderr) != "warn" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "warn")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}

	c := fb.lastExec(t)
	if len(c.Argv) != 2 || c.Argv[0] != "echo" || c.Argv[1] != "x" {
		t.Errorf("argv = %v, want [echo x]", c.Argv)
	}
	if c.Cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", c.Cwd)
	}
	// The bridge exec body is exactly {argv, cwd, timeout_ms}.
	if len(c.Body) != 3 {
		t.Errorf("body keys = %v, want exactly argv/cwd/timeout_ms", keysOf(c.Body))
	}
	if _, ok := c.Body["env"]; ok {
		t.Error("body must not carry env (bridge API has no env field)")
	}
	if _, ok := c.Body["stdin"]; ok {
		t.Error("body must not carry stdin (bridge API has no stdin field)")
	}
	if c.TimeoutMs <= 0 {
		t.Errorf("timeout_ms = %d, want > 0 (derived from ctx deadline)", c.TimeoutMs)
	}
	fb.requireAuth(t, "Bearer k1")
}

func TestExecNonZeroExitIsAResult(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stderr("boom")
		w.exit(3)
	}
	b := newBridge(t, fb)
	res, err := b.Exec(t.Context(), executor.ExecRequest{Name: "false"})
	if err != nil {
		t.Fatalf("Exec: %v (non-zero exit must not be an error)", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", res.ExitCode)
	}
	if string(res.Stderr) != "boom" {
		t.Fatalf("stderr = %q, want boom", res.Stderr)
	}
}

func TestExecErrorEvent(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.event("error", `{"error":"disk full","code":"ENOSPC"}`)
	}
	b := newBridge(t, fb)
	res, err := b.Exec(t.Context(), executor.ExecRequest{Name: "x"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != -1 {
		t.Fatalf("exit = %d, want -1", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "disk full") {
		t.Fatalf("stderr = %q, want it to contain the error message", res.Stderr)
	}
}

func TestExecStreamWithoutExitEvent(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("partial")
	}
	b := newBridge(t, fb)
	_, err := b.Exec(t.Context(), executor.ExecRequest{Name: "x"})
	if err == nil {
		t.Fatal("want error when the stream ends without an exit event")
	}
	if !strings.Contains(err.Error(), "without exit") {
		t.Fatalf("err = %v, want it to mention the missing exit event", err)
	}
}

func TestExecHTTPError(t *testing.T) {
	fb := newFake(t)
	fb.mu.Lock()
	fb.execStatus = 500
	fb.execBody = `{"error":"sandbox gone"}`
	fb.mu.Unlock()
	b := newBridge(t, fb)
	_, err := b.Exec(t.Context(), executor.ExecRequest{Name: "x"})
	if err == nil {
		t.Fatal("want error on http 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "sandbox gone") {
		t.Fatalf("err = %v, want it to carry status and body", err)
	}
}

func TestExecNonSSEContentType(t *testing.T) {
	fb := newFake(t)
	fb.mu.Lock()
	fb.execCT = "application/json"
	fb.mu.Unlock()
	b := newBridge(t, fb)
	_, err := b.Exec(t.Context(), executor.ExecRequest{Name: "x"})
	if err == nil {
		t.Fatal("want error when exec does not answer text/event-stream")
	}
}

// --- Start / Process ---------------------------------------------------------

func TestStartStreamsUntilExit(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("stream-")
		w.stdout("out")
		w.exit(0)
	}
	b := newBridge(t, fb)
	p, err := b.Start(t.Context(), executor.ExecRequest{Name: "tail", Args: []string{"-f", "log"}}, executor.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if string(out) != "stream-out" {
		t.Fatalf("stdout = %q, want stream-out", out)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// The start body is {argv, cwd} only: no timeout_ms, the SSE stream is
	// the process lifetime.
	c := fb.lastExec(t)
	if len(c.Body) != 2 {
		t.Fatalf("body keys = %v, want exactly argv/cwd", keysOf(c.Body))
	}
	if _, ok := c.Body["timeout_ms"]; ok {
		t.Error("start body must not carry timeout_ms")
	}
	if len(c.Argv) != 3 || c.Argv[0] != "tail" || c.Argv[1] != "-f" || c.Argv[2] != "log" {
		t.Errorf("argv = %v, want [tail -f log]", c.Argv)
	}
}

func TestStartNonZeroExit(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.exit(7)
	}
	b := newBridge(t, fb)
	p, err := b.Start(t.Context(), executor.ExecRequest{Name: "x"}, executor.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	werr := p.Wait()
	if werr == nil {
		t.Fatal("Wait = nil, want *ExitError")
	}
	ee, ok := werr.(*executor.ExitError)
	if !ok || ee.Code != 7 {
		t.Fatalf("Wait = %v, want *ExitError{7}", werr)
	}
}

func TestStartKill(t *testing.T) {
	fb := newFake(t)
	release := make(chan struct{})
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("hi")
		select {
		case <-w.ctx.Done(): // client aborted the stream (Kill)
		case <-release:
			w.exit(0)
		}
	}
	b := newBridge(t, fb)
	p, err := b.Start(t.Context(), executor.ExecRequest{Name: "server"}, executor.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(p.Stdout(), buf); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait after Kill = %v, want nil", err)
	}
	close(release)
}

func TestStartCtxCancelKills(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("hi")
		<-w.ctx.Done()
	}
	b := newBridge(t, fb)
	ctx, cancel := context.WithCancel(t.Context())
	p, err := b.Start(ctx, executor.ExecRequest{Name: "server"}, executor.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(p.Stdout(), buf); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	cancel()
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait after ctx cancel = %v, want nil", err)
	}
}

func TestStartOptsRedirect(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("A")
		w.stderr("B")
		w.exit(0)
	}
	b := newBridge(t, fb)
	var out, errb bytes.Buffer
	p, err := b.Start(t.Context(), executor.ExecRequest{Name: "x"}, executor.StartOptions{Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.Stdout() != nil {
		t.Fatal("Stdout() must be nil when StartOptions.Stdout is set")
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out.String() != "A" {
		t.Fatalf("opts.Stdout = %q, want A", out.String())
	}
	if errb.String() != "B" {
		t.Fatalf("opts.Stderr = %q, want B", errb.String())
	}
}

func TestStartStdinIsDiscarded(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.exit(0)
	}
	b := newBridge(t, fb)
	p, err := b.Start(t.Context(), executor.ExecRequest{Name: "x"}, executor.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sw := p.Stdin()
	n, err := sw.Write([]byte("ignored"))
	if err != nil || n != len("ignored") {
		t.Fatalf("Stdin write = %d, %v; writes must be accepted and discarded", n, err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Stdin close: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// --- file surface ------------------------------------------------------------

func TestWriteReadFileRaw(t *testing.T) {
	fb := newFake(t)
	b := newBridge(t, fb)
	if err := b.WriteFile(t.Context(), "/workspace/a b/c.txt", []byte("raw-bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fb.mu.Lock()
	puts, putCT := fb.filePuts, fb.putCT
	fb.mu.Unlock()
	if len(puts) != 1 || puts[0] != "workspace/a b/c.txt" {
		t.Fatalf("file puts = %v, want [workspace/a b/c.txt]", puts)
	}
	if putCT != "application/octet-stream" {
		t.Fatalf("put content-type = %q, want application/octet-stream", putCT)
	}
	data, err := b.ReadFile(t.Context(), "/workspace/a b/c.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "raw-bytes" {
		t.Fatalf("read = %q, want raw-bytes", data)
	}
	// The path segments were escaped per segment on the wire.
	fb.mu.Lock()
	getFile := fb.fileGetPath
	fb.mu.Unlock()
	if !strings.Contains(getFile, "a%20b") {
		t.Fatalf("GET path = %q, want per-segment escaping of the space", getFile)
	}
	if _, err := b.ReadFile(t.Context(), "/workspace/missing.txt"); err == nil {
		t.Fatal("ReadFile of a missing file must error")
	}
	fb.requireAuth(t, "Bearer k1")
}

func TestStat(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		if len(call.Argv) > 0 && call.Argv[0] == "stat" && call.Argv[3] == "/workspace/dir" {
			w.stdout("755 4096 1700000000 directory")
		} else {
			w.stdout("644 12 1699999999 regular file")
		}
		w.exit(0)
	}
	b := newBridge(t, fb)
	fi, err := b.Stat(t.Context(), "/workspace/x.go")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Name() != "x.go" {
		t.Errorf("Name = %q, want x.go", fi.Name())
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("Mode = %v, want 0644", fi.Mode())
	}
	if fi.IsDir() {
		t.Error("IsDir = true, want false")
	}
	if fi.Size() != 12 {
		t.Errorf("Size = %d, want 12", fi.Size())
	}
	if fi.ModTime().Unix() != 1699999999 {
		t.Errorf("ModTime = %v, want unix 1699999999", fi.ModTime())
	}
	// Pinned command shape: stat -c "%a %s %Y %F" <path>
	if got := fb.lastExec(t).Argv; len(got) != 4 || got[0] != "stat" || got[1] != "-c" || got[2] != "%a %s %Y %F" || got[3] != "/workspace/x.go" {
		t.Errorf("stat argv = %v", got)
	}

	fi, err = b.Stat(t.Context(), "/workspace/dir")
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeDir == 0 {
		t.Fatalf("dir stat: IsDir=%v Mode=%v", fi.IsDir(), fi.Mode())
	}
}

func TestStatMissingFile(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stderr("stat: cannot statx '/workspace/nope': No such file or directory")
		w.exit(1)
	}
	b := newBridge(t, fb)
	_, err := b.Stat(t.Context(), "/workspace/nope")
	if err == nil {
		t.Fatal("Stat of a missing file must error")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("err = %v, want it to carry stderr", err)
	}
}

func TestMkdirAll(t *testing.T) {
	fb := newFake(t)
	b := newBridge(t, fb)
	if err := b.MkdirAll(t.Context(), "/workspace/d/e", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Pinned command shape: mkdir -p <path>
	if got := fb.lastExec(t).Argv; len(got) != 3 || got[0] != "mkdir" || got[1] != "-p" || got[2] != "/workspace/d/e" {
		t.Fatalf("mkdir argv = %v, want [mkdir -p /workspace/d/e]", got)
	}
}

func TestGlob(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("/workspace/a.go\n/workspace/sub/b.go\n/workspace/c.txt\n")
		w.exit(0)
	}
	b := newBridge(t, fb)
	m, err := b.Glob(t.Context(), "/workspace/*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(m) != 1 || m[0] != "/workspace/a.go" {
		t.Fatalf("matches = %v, want [/workspace/a.go] (* must not cross /)", m)
	}
	// Pinned command shape: find <root> -path <pattern>
	if got := fb.lastExec(t).Argv; len(got) != 4 || got[0] != "find" || got[1] != "/workspace" || got[2] != "-path" || got[3] != "/workspace/*.go" {
		t.Fatalf("find argv = %v", got)
	}
}

func TestGrep(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("/workspace/a.go\x003:TODO fix\n/workspace/b.go\x007:TODO more\n")
		w.exit(0)
	}
	b := newBridge(t, fb)
	ms, err := b.Grep(t.Context(), "/workspace", regexp.MustCompile("TODO"))
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("matches = %v, want 2", ms)
	}
	if ms[0].Path != "/workspace/a.go" || ms[0].Line != 3 || ms[0].Text != "TODO fix" {
		t.Errorf("match[0] = %+v", ms[0])
	}
	if ms[1].Path != "/workspace/b.go" || ms[1].Line != 7 || ms[1].Text != "TODO more" {
		t.Errorf("match[1] = %+v", ms[1])
	}
	// Pinned command shape (GNU grep NUL format: path\0line:text).
	want := []string{"grep", "-r", "-n", "-E", "-I", "--null", "--exclude-dir=.*", "TODO", "/workspace"}
	if got := fb.lastExec(t).Argv; !equalStrings(got, want) {
		t.Fatalf("grep argv = %v, want %v", got, want)
	}
}

func TestGrepNoMatchIsNotAnError(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.exit(1) // grep exits 1 when nothing matched
	}
	b := newBridge(t, fb)
	ms, err := b.Grep(t.Context(), "/workspace", regexp.MustCompile("zzz"))
	if err != nil {
		t.Fatalf("Grep: %v (grep exit 1 means no matches)", err)
	}
	if len(ms) != 0 {
		t.Fatalf("matches = %v, want empty", ms)
	}
}

func TestGrepColonFallback(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stdout("/workspace/a.go:3:TODO fix\n")
		w.exit(0)
	}
	b := newBridge(t, fb)
	ms, err := b.Grep(t.Context(), "/workspace", regexp.MustCompile("TODO"))
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(ms) != 1 || ms[0].Path != "/workspace/a.go" || ms[0].Line != 3 || ms[0].Text != "TODO fix" {
		t.Fatalf("matches = %+v", ms)
	}
}

func TestGrepCappedAt200(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		var sb strings.Builder
		for i := 0; i < 205; i++ {
			fmt.Fprintf(&sb, "/workspace/f.go\x00%d:TODO\n", i+1)
		}
		w.stdout(sb.String())
		w.exit(0)
	}
	b := newBridge(t, fb)
	ms, err := b.Grep(t.Context(), "/workspace", regexp.MustCompile("TODO"))
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(ms) != 200 {
		t.Fatalf("matches = %d, want 200 (contract max_matches cap)", len(ms))
	}
}

func TestGrepError(t *testing.T) {
	fb := newFake(t)
	fb.execFn = func(call execCall, w sseResp) {
		w.stderr("grep: /workspace: Permission denied")
		w.exit(2)
	}
	b := newBridge(t, fb)
	_, err := b.Grep(t.Context(), "/workspace", regexp.MustCompile("x"))
	if err == nil {
		t.Fatal("want error on grep exit 2")
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v, want it to carry stderr", err)
	}
}

// --- archives (persist / hydrate) --------------------------------------------

func TestArchiveRoundtrip(t *testing.T) {
	fb := newFake(t)
	b := newBridge(t, fb)

	// Build a raw tar with one executable file, then gzip it: the caller
	// side of PutArchive speaks contract-v2 gzip tars.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	content := "hello archive\n"
	if err := tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := b.PutArchive(t.Context(), &gz); err != nil {
		t.Fatalf("PutArchive: %v", err)
	}
	fb.mu.Lock()
	hydrates, hydrateCT := fb.hydrates, fb.hydrateCT
	fb.mu.Unlock()
	if len(hydrates) != 1 {
		t.Fatalf("hydrate calls = %d, want 1", len(hydrates))
	}
	if hydrateCT != "application/x-tar" {
		t.Fatalf("hydrate content-type = %q, want application/x-tar", hydrateCT)
	}
	// The bridge receives the RAW uncompressed tar.
	if !bytes.Equal(hydrates[0], raw.Bytes()) {
		t.Fatal("hydrate body is not the raw tar of the gzip input")
	}

	rc, err := b.GetArchive(t.Context())
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	defer func() { _ = rc.Close() }()
	// The caller receives gzip again, wrapping the bridge's raw tar.
	gzr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("GetArchive body is not gzip: %v", err)
	}
	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if hdr.Name != "hello.txt" || hdr.Mode != 0o755 {
		t.Fatalf("tar header = %+v, want hello.txt 0755", hdr)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("tar read: %v", err)
	}
	if string(got) != content {
		t.Fatalf("tar content = %q, want %q", got, content)
	}
	fb.mu.Lock()
	persistQ := fb.persistQ
	fb.mu.Unlock()
	if persistQ != "excludes=.git" {
		t.Fatalf("persist query = %q, want excludes=.git", persistQ)
	}
}

func TestHydrateTooLarge(t *testing.T) {
	fb := newFake(t)
	b := newBridge(t, fb)

	// A raw tar just over the 32 MiB bridge cap, all zeros so the gzip
	// stream stays small.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	size := int64(maxHydrateBytes + 1)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	err := b.PutArchive(t.Context(), &gz)
	if err == nil {
		t.Fatal("want error when the raw tar exceeds the bridge hydrate cap")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want it to mention the size cap", err)
	}
	fb.mu.Lock()
	n := len(fb.hydrates)
	fb.mu.Unlock()
	if n != 0 {
		t.Fatalf("hydrate calls = %d, want 0 (upload must be rejected client-side)", n)
	}
}

// --- helpers -----------------------------------------------------------------

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
