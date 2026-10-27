package executor

// Tests for the contract v2 sandbox HTTP service (service.go/sessions.go):
// every endpoint is served over a real httptest server backed by the
// FakeBackend, so the whole surface runs without any Docker daemon. The
// flow tests follow the contract document (internal/platform/executor/
// CONTRACT.md) shape for shape: base64 payloads, env objects, 204s for
// mutations, 404 for unknown ids, 400 for malformed requests.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	nacosrt "github.com/projanvil/woca/internal/ee/nacosrt"
)

// --- helpers ---------------------------------------------------------------

type svcEvent struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

type svcOutput struct {
	Events     []svcEvent `json:"events"`
	Done       bool       `json:"done"`
	ExitCode   int        `json:"exit_code"`
	NextOffset int        `json:"next_offset"`
}

// newTestConfig returns a service config over a fresh FakeBackend and
// workspace store; mutate tweaks fields before startTestService.
func newTestConfig(t *testing.T) (*ServiceConfig, *FakeBackend) {
	t.Helper()
	backend, err := NewFakeBackend()
	if err != nil {
		t.Fatalf("NewFakeBackend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	cfg := &ServiceConfig{
		Backend: backend,
		Store:   NewWorkspaceStore(t.TempDir(), 64<<20),
		Logger:  zerolog.New(io.Discard),
		Options: func() nacosrt.BackendOptions {
			return nacosrt.BackendOptions{Image: "woca-sandbox-test:1"}
		},
	}
	return cfg, backend
}

func startTestService(t *testing.T, cfg *ServiceConfig) (*Service, *httptest.Server) {
	t.Helper()
	svc := NewService(*cfg).(*Service)
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = svc.Close() })
	return svc, srv
}

// call performs a JSON request; body nil means no body, out non-nil decodes
// the response. Returns the status code.
func call(t *testing.T, srv *httptest.Server, method, p string, body, out any) int {
	t.Helper()
	return callAuth(t, srv, method, p, "", body, out)
}

// callAuth is call with an optional bearer token.
func callAuth(t *testing.T, srv *httptest.Server, method, p, bearer string, body, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+p, rdr)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, p, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response (status %d): %v", method, p, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// callBytes performs a raw request and returns status, body and headers.
func callBytes(t *testing.T, srv *httptest.Server, method, p, contentType string, body []byte, bearer string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+p, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, p, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, p, err)
	}
	return resp.StatusCode, data, resp.Header
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func unb64(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64 %q: %v", s, err)
	}
	return string(raw)
}

// createSandbox provisions a sandbox for workspace ("" = not routed) and
// returns its id.
func createSandbox(t *testing.T, srv *httptest.Server, workspace string) string {
	t.Helper()
	body := map[string]any{"cwd": "/workspace"}
	if workspace != "" {
		body["workspace"] = workspace
	}
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/create", body, &out); code != http.StatusOK {
		t.Fatalf("create sandbox %q: status %d", workspace, code)
	}
	if out.SandboxID == "" {
		t.Fatal("create sandbox: empty sandbox_id")
	}
	return out.SandboxID
}

// execSandbox runs a synchronous exec and returns (exit code, stdout).
func execSandbox(t *testing.T, srv *httptest.Server, sbx string, argv []string) (int, string) {
	t.Helper()
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{
		"argv": argv, "cwd": "/workspace", "env": map[string]string{},
	}, &out)
	if code != http.StatusOK {
		t.Fatalf("exec %v: status %d", argv, code)
	}
	return out.ExitCode, unb64(t, out.Stdout)
}

// startSession starts argv and returns the session id.
func startSession(t *testing.T, srv *httptest.Server, sbx string, argv ...string) string {
	t.Helper()
	var out struct {
		SessionID string `json:"session_id"`
	}
	code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/start", map[string]any{
		"argv": argv, "cwd": "/workspace", "env": map[string]string{},
	}, &out)
	if code != http.StatusOK {
		t.Fatalf("start %v: status %d", argv, code)
	}
	if out.SessionID == "" {
		t.Fatal("start: empty session_id")
	}
	return out.SessionID
}

// pollUntilDone polls a session's output from offset 0, accumulating events
// and asserting next_offset never moves backwards, until done. Returns the
// final response and all events seen.
func pollUntilDone(t *testing.T, srv *httptest.Server, sbx, sid string, timeout time.Duration) (svcOutput, []svcEvent) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	offset := 0
	var all []svcEvent
	next := -1
	for {
		var out svcOutput
		p := fmt.Sprintf("/sandbox/%s/sessions/%s/output?offset=%d", sbx, sid, offset)
		if code := call(t, srv, http.MethodGet, p, nil, &out); code != http.StatusOK {
			t.Fatalf("output %s: status %d", p, code)
		}
		if out.NextOffset < next {
			t.Fatalf("next_offset went backwards: %d < %d", out.NextOffset, next)
		}
		next = out.NextOffset
		all = append(all, out.Events...)
		if out.Done {
			return out, all
		}
		offset = out.NextOffset
		if time.Now().After(deadline) {
			t.Fatalf("session %s not done after %v", sid, timeout)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// withSessionCap temporarily shrinks the session output cap.
func withSessionCap(t *testing.T, cap int) {
	t.Helper()
	old := sessionOutputCap
	sessionOutputCap = cap
	t.Cleanup(func() { sessionOutputCap = old })
}

func withSessionTTL(t *testing.T, ttl time.Duration) {
	t.Helper()
	old := sessionIdleTTL
	sessionIdleTTL = ttl
	t.Cleanup(func() { sessionIdleTTL = old })
}

// --- full 15-endpoint flow ---------------------------------------------------

func TestServiceFullFlow(t *testing.T) {
	cfg, backend := newTestConfig(t)
	_, srv := startTestService(t, cfg)

	// Seed the host workspace before create; the sandbox bind-mounts the
	// same directory (spec §5.3), so the file is visible in-container with
	// no upload step.
	dir, err := cfg.Store.Dir("ws_1")
	if err != nil {
		t.Fatalf("Store.Dir: %v", err)
	}
	writeTestFile(t, dir+"/hello.txt", "hello woca\n")

	// 1. create
	sbx := createSandbox(t, srv, "ws_1")

	// 2. exec: mounted file visible, env + stdin plumbing work. The fake
	// backend maps the container cwd onto the mounted host directory but
	// does not rewrite argv paths, so exec tests address files relatively —
	// a real container would accept "/workspace/hello.txt" just as well.
	if code, out := execSandbox(t, srv, sbx, []string{"cat", "hello.txt"}); code != 0 || out != "hello woca\n" {
		t.Fatalf("exec cat mounted file: exit %d out %q", code, out)
	}
	var execOut struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{
		"argv": []string{"sh", "-c", "printf %s \"$WOCATOK\""},
		"cwd":  "/workspace", "env": map[string]string{"WOCATOK": "token-ok"},
		"stdin": b64("via-stdin"),
	}, &execOut); code != http.StatusOK {
		t.Fatalf("exec env+stdin: status %d", code)
	}
	if execOut.ExitCode != 0 || unb64(t, execOut.Stdout) != "token-ok" {
		t.Fatalf("exec env: exit %d stdout %q", execOut.ExitCode, unb64(t, execOut.Stdout))
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{
		"argv": []string{"cat"}, "cwd": "/workspace", "env": map[string]string{},
		"stdin": b64("piped-payload"),
	}, &execOut); code != http.StatusOK || unb64(t, execOut.Stdout) != "piped-payload" {
		t.Fatalf("exec stdin: status ? stdout %q", unb64(t, execOut.Stdout))
	}
	// Non-zero exit is data, not an HTTP error.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{
		"argv": []string{"sh", "-c", "echo oops >&2; exit 3"}, "cwd": "/workspace",
		"env": map[string]string{},
	}, &execOut); code != http.StatusOK || execOut.ExitCode != 3 {
		t.Fatalf("exec exit passthrough: status %d exit %d", code, execOut.ExitCode)
	}
	if unb64(t, execOut.Stderr) != "oops\n" {
		t.Fatalf("exec stderr: %q", unb64(t, execOut.Stderr))
	}

	// 8. write → 7. read round-trip.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{
		"path": "/workspace/written.txt", "content": b64("written-by-endpoint"), "mode": 420,
	}, nil); code != http.StatusNoContent {
		t.Fatalf("write: status %d", code)
	}
	var readOut struct {
		Content string `json:"content"`
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/read?path="+"/workspace/written.txt", nil, &readOut); code != http.StatusOK {
		t.Fatalf("read: status %d", code)
	}
	if unb64(t, readOut.Content) != "written-by-endpoint" {
		t.Fatalf("read: %q", unb64(t, readOut.Content))
	}

	// 9. stat (file and directory).
	var statOut struct {
		Mode    int64 `json:"mode"`
		Size    int64 `json:"size"`
		MtimeMs int64 `json:"mtime_ms"`
		IsDir   bool  `json:"is_dir"`
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/stat?path="+"/workspace/written.txt", nil, &statOut); code != http.StatusOK {
		t.Fatalf("stat file: status %d", code)
	}
	if statOut.IsDir || statOut.Size != int64(len("written-by-endpoint")) || statOut.Mode != 420 {
		t.Fatalf("stat file: %+v", statOut)
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/stat?path=/workspace", nil, &statOut); code != http.StatusOK || !statOut.IsDir {
		t.Fatalf("stat dir: status ? is_dir %v", statOut.IsDir)
	}

	// 10. mkdir (parents) + 11. glob.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/mkdir", map[string]any{"path": "/workspace/deep/dir"}, nil); code != http.StatusNoContent {
		t.Fatalf("mkdir: status %d", code)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{
		"path": "/workspace/deep/file.txt", "content": b64("needle"), "mode": 420,
	}, nil); code != http.StatusNoContent {
		t.Fatalf("write nested: status %d", code)
	}
	var globOut struct {
		Matches []string `json:"matches"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/glob", map[string]any{"pattern": "/workspace/deep/*"}, &globOut); code != http.StatusOK {
		t.Fatalf("glob: status %d", code)
	}
	want := []string{"/workspace/deep/dir", "/workspace/deep/file.txt"}
	if len(globOut.Matches) != len(want) {
		t.Fatalf("glob matches: %v", globOut.Matches)
	}
	for i := range want {
		if globOut.Matches[i] != want[i] {
			t.Fatalf("glob matches: %v", globOut.Matches)
		}
	}

	// 12. grep.
	var grepOut struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/grep", map[string]any{
		"root": "/workspace", "pattern": "needle|hello", "max_matches": 200,
	}, &grepOut); code != http.StatusOK {
		t.Fatalf("grep: status %d", code)
	}
	if len(grepOut.Matches) != 2 {
		t.Fatalf("grep matches: %+v", grepOut.Matches)
	}
	byText := map[string]string{}
	for _, m := range grepOut.Matches {
		byText[m.Path] = unb64(t, m.Text)
	}
	if byText["/workspace/hello.txt"] != "hello woca" || byText["/workspace/deep/file.txt"] != "needle" {
		t.Fatalf("grep matched lines: %v", byText)
	}

	// 3. start → 5. input (+eof) → 4. output → done.
	sid := startSession(t, srv, sbx, "cat")
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/sessions/"+sid+"/input", map[string]any{"data": b64("ping")}, nil); code != http.StatusNoContent {
		t.Fatalf("input: status %d", code)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/sessions/"+sid+"/input", map[string]any{"eof": true}, nil); code != http.StatusNoContent {
		t.Fatalf("input eof: status %d", code)
	}
	final, events := pollUntilDone(t, srv, sbx, sid, 5*time.Second)
	if final.ExitCode != 0 {
		t.Fatalf("cat session exit: %d", final.ExitCode)
	}
	var echo string
	for _, e := range events {
		if e.Stream == "stdout" {
			echo += unb64(t, e.Data)
		}
	}
	if echo != "ping" {
		t.Fatalf("cat session stdout: %q", echo)
	}

	// 6. kill on a live session, then a no-op kill after exit.
	sid2 := startSession(t, srv, sbx, "sleep", "30")
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/sessions/"+sid2+"/kill", map[string]any{}, nil); code != http.StatusNoContent {
		t.Fatalf("kill: status %d", code)
	}
	final2, _ := pollUntilDone(t, srv, sbx, sid2, 5*time.Second)
	if !final2.Done || final2.ExitCode == 0 {
		t.Fatalf("killed session: done %v exit %d", final2.Done, final2.ExitCode)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/sessions/"+sid2+"/kill", map[string]any{}, nil); code != http.StatusNoContent {
		t.Fatalf("kill after exit: status %d", code)
	}
	// Input after exit is a no-op.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/sessions/"+sid2+"/input", map[string]any{"data": b64("x")}, nil); code != http.StatusNoContent {
		t.Fatalf("input after exit: status %d", code)
	}

	// The mount (spec §5.3): a file written through the sandbox's write
	// endpoint is on the host workspace directory immediately — no sync
	// call, the sandbox works on the volume in place.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{
		"path": "/workspace/synced.txt", "content": b64("synced-payload"), "mode": 420,
	}, nil); code != http.StatusNoContent {
		t.Fatalf("write for mount check: status %d", code)
	}
	host, err := cfg.Store.Dir("ws_1")
	if err != nil {
		t.Fatalf("Store.Dir after write: %v", err)
	}
	raw, err := os.ReadFile(host + "/synced.txt")
	if err != nil || string(raw) != "synced-payload" {
		t.Fatalf("host synced.txt: %q err %v", raw, err)
	}
	// The sync endpoint is gone with the transfer machinery: an explicit
	// 404 for a live sandbox, not a silent no-op.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/workspace/sync", nil, nil); code != http.StatusNotFound {
		t.Fatalf("workspace/sync on a live sandbox: status %d (want 404, the endpoint is removed)", code)
	}

	// 15. destroy: idempotent 2xx, sessions die, container gone, lock freed.
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbx, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy: status %d", code)
	}
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbx, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy again: status %d", code)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{"argv": []string{"true"}}, nil); code != http.StatusNotFound {
		t.Fatalf("exec after destroy: status %d", code)
	}
	live, err := backend.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("containers after destroy: %d", len(live))
	}
	lock, err := LockDir(dir)
	if err != nil {
		t.Fatalf("workspace lock after destroy: %v", err)
	}
	_ = lock.Close()
}

// --- workspace lock and validation ------------------------------------------

func TestServiceCreateWorkspaceBusy(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)

	sbxA := createSandbox(t, srv, "ws_dup")
	body := map[string]any{"workspace": "ws_dup", "cwd": "/workspace"}
	var errOut struct {
		Error string `json:"error"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/create", body, &errOut); code != http.StatusServiceUnavailable {
		t.Fatalf("second create same workspace: status %d (want 503)", code)
	}
	if errOut.Error == "" {
		t.Fatal("busy error body missing error field")
	}
	// A different workspace is unaffected while the first is live.
	createSandbox(t, srv, "ws_other")
	// Destroy releases the lock; the workspace is creatable again.
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbxA, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy: status %d", code)
	}
	createSandbox(t, srv, "ws_dup")
}

func TestServiceCreateInvalidWorkspace(t *testing.T) {
	cfg, backend := newTestConfig(t)
	_, srv := startTestService(t, cfg)

	for _, ws := range []string{"../etc", "/abs/path", "a b", "dot.name", "semi;colon"} {
		if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"workspace": ws, "cwd": "/workspace"}, nil); code != http.StatusBadRequest {
			t.Fatalf("create workspace %q: status %d (want 400)", ws, code)
		}
	}
	live, err := backend.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("containers after invalid creates: %d", len(live))
	}
	// Empty workspace is legal (not workspace-routed).
	createSandbox(t, srv, "")
}

func TestServiceCreateHeaderWorkspace(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.Labels = func(runID string) map[string]string {
		return map[string]string{LabelSandbox: "1", LabelWorkspace: runID}
	}
	_, srv := startTestService(t, cfg)

	dir, err := cfg.Store.Dir("ws_hdr")
	if err != nil {
		t.Fatalf("Store.Dir: %v", err)
	}
	writeTestFile(t, dir+"/marker.txt", "from-header\n")
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	req := map[string]any{"cwd": "/workspace"} // workspace rides the header only
	if code := callH(t, srv, http.MethodPost, "/sandbox/create", "ws_hdr", req, &out); code != http.StatusOK {
		t.Fatalf("create with header workspace: status %d", code)
	}
	if _, got := execSandbox(t, srv, out.SandboxID, []string{"cat", "marker.txt"}); got != "from-header\n" {
		t.Fatalf("header workspace mount: %q", got)
	}
}

// callH is call with an X-Woca-Workspace routing header.
func callH(t *testing.T, srv *httptest.Server, method, p, workspace string, body, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+p, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Woca-Workspace", workspace)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode
}

// --- create failure semantics ---------------------------------------------------

// A backend that cannot mount (or otherwise refuses Create) fails the
// create with 500, leaves no container behind, and releases the workspace
// lock: nothing stays wedged (spec §10.2's cleanup posture, now without
// any upload step to fail).
func TestServiceCreateFailureCleansUp(t *testing.T) {
	cfg, backend := newTestConfig(t)
	cfg.Backend = &failingBackend{Backend: cfg.Backend, createErr: errors.New("mount boom")}
	_, srv := startTestService(t, cfg)

	dir, err := cfg.Store.Dir("ws_mount")
	if err != nil {
		t.Fatalf("Store.Dir: %v", err)
	}
	writeTestFile(t, dir+"/big.txt", strings.Repeat("x", 128))
	var errOut struct {
		Error string `json:"error"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_mount", "cwd": "/workspace"}, &errOut); code != http.StatusInternalServerError {
		t.Fatalf("create with failing backend: status %d (want 500)", code)
	}
	if errOut.Error == "" {
		t.Fatal("create failure error body empty")
	}
	live, err := backend.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("containers after failed create: %d", len(live))
	}
	// The workspace lock must be released and the host dir untouched.
	lock, err := LockDir(dir)
	if err != nil {
		t.Fatalf("workspace lock after failed create: %v", err)
	}
	_ = lock.Close()
	if b, err := os.ReadFile(dir + "/big.txt"); err != nil || string(b) != strings.Repeat("x", 128) {
		t.Fatalf("host dir disturbed by failed create: (%d bytes, %v)", len(b), err)
	}
}

// --- authentication ------------------------------------------------------------

func TestServiceAuth(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.APIKey = "sekrit"
	_, srv := startTestService(t, cfg)

	if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"cwd": "/workspace"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("create without bearer: status %d (want 401)", code)
	}
	if code := callAuth(t, srv, http.MethodPost, "/sandbox/create", "wrong", map[string]any{"cwd": "/workspace"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("create with wrong bearer: status %d (want 401)", code)
	}
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	if code := callAuth(t, srv, http.MethodPost, "/sandbox/create", "sekrit", map[string]any{"cwd": "/workspace"}, &out); code != http.StatusOK {
		t.Fatalf("create with correct bearer: status %d", code)
	}
	// The internal endpoint is authenticated too.
	if code := callAuth(t, srv, http.MethodGet, "/internal/sandboxes", "wrong", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("internal list with wrong bearer: status %d", code)
	}
	if code := callAuth(t, srv, http.MethodGet, "/internal/sandboxes", "sekrit", nil, nil); code != http.StatusOK {
		t.Fatalf("internal list with correct bearer: status %d", code)
	}
	// Sandbox-scoped endpoints keep requiring the key.
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+out.SandboxID, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("destroy without bearer: status %d", code)
	}
}

// --- internal listing -----------------------------------------------------------

func TestServiceInternalSandboxes(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.Labels = func(runID string) map[string]string {
		return map[string]string{LabelSandbox: "1", LabelWorkspace: runID}
	}
	_, srv := startTestService(t, cfg)

	a := createSandbox(t, srv, "ws_a")
	b := createSandbox(t, srv, "ws_b")

	var list struct {
		Sandboxes []struct {
			ID      string            `json:"id"`
			Running bool              `json:"running"`
			Labels  map[string]string `json:"labels"`
		} `json:"sandboxes"`
	}
	if code := call(t, srv, http.MethodGet, "/internal/sandboxes", nil, &list); code != http.StatusOK {
		t.Fatalf("internal list: status %d", code)
	}
	if len(list.Sandboxes) != 2 {
		t.Fatalf("internal list: %d entries", len(list.Sandboxes))
	}
	if got := list.Sandboxes[0].Labels[LabelWorkspace]; got != "ws_a" {
		t.Fatalf("labels: %v", list.Sandboxes[0].Labels)
	}

	if code := call(t, srv, http.MethodGet, "/internal/sandboxes?workspace=ws_a", nil, &list); code != http.StatusOK || len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != a {
		t.Fatalf("filter by workspace: code=%d entries=%d", code, len(list.Sandboxes))
	}
	if code := call(t, srv, http.MethodGet, "/internal/sandboxes?run=r9", nil, &list); code != http.StatusOK || len(list.Sandboxes) != 0 {
		t.Fatalf("filter by unknown run: code=%d entries=%d", code, len(list.Sandboxes))
	}
	if code := call(t, srv, http.MethodGet, "/internal/sandboxes?workspace=ws_a&run=r9", nil, &list); code != http.StatusOK || len(list.Sandboxes) != 0 {
		t.Fatalf("combined filter: code=%d entries=%d", code, len(list.Sandboxes))
	}

	if code := call(t, srv, http.MethodDelete, "/sandbox/"+b, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy: status %d", code)
	}
	if code := call(t, srv, http.MethodGet, "/internal/sandboxes", nil, &list); code != http.StatusOK || len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != a {
		t.Fatalf("after destroy: code=%d entries=%d", code, len(list.Sandboxes))
	}
}

// --- capacity ---------------------------------------------------------------------

func TestServiceMaxConcurrent(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.MaxConcurrent = 1
	_, srv := startTestService(t, cfg)

	createSandbox(t, srv, "ws_cap")
	if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_cap2", "cwd": "/workspace"}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("create over capacity: status %d (want 503)", code)
	}

	// The cfg.ActiveRuns hook never gates admission: it counts runs for
	// the ack heartbeat, a different unit than the sandbox cap. A
	// saturated hook with zero live sandboxes must still admit creates.
	cfg2, _ := newTestConfig(t)
	cfg2.MaxConcurrent = 1
	cfg2.ActiveRuns = func() int { return 99 }
	svc2, srv2 := startTestService(t, cfg2)
	sbx2 := createSandbox(t, srv2, "") // saturated hook, zero sandboxes → 200
	// The admission counter is the registry itself.
	if got := svc2.ActiveRuns(); got != 1 {
		t.Fatalf("ActiveRuns after create: %d (want 1)", got)
	}
	if code := call(t, srv2, http.MethodDelete, "/sandbox/"+sbx2, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy: status %d", code)
	}
	if got := svc2.ActiveRuns(); got != 0 {
		t.Fatalf("ActiveRuns after destroy: %d (want 0)", got)
	}
}

// --- exec timeout -------------------------------------------------------------------

func TestServiceExecTimeout(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_t")

	start := time.Now()
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/exec", map[string]any{
		"argv": []string{"sleep", "10"}, "cwd": "/workspace", "env": map[string]string{},
		"timeout_ms": 300,
	}, nil); code != http.StatusInternalServerError {
		t.Fatalf("exec timeout: status %d (want 500)", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("exec timeout took %v (kill not applied?)", elapsed)
	}
}

// --- session output truncation --------------------------------------------------------

func TestServiceOutputFloodTruncation(t *testing.T) {
	withSessionCap(t, 4096)
	cfg, _ := newTestConfig(t)
	svc, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_flood")

	const total = 64 * 1024
	flood := startSession(t, srv, sbx, "sh", "-c", "dd if=/dev/zero bs=1024 count=64 2>/dev/null")
	sid := flood
	final, _ := pollUntilDone(t, srv, sbx, sid, 10*time.Second)

	if final.NextOffset != total {
		t.Fatalf("next_offset after flood: %d (want %d)", final.NextOffset, total)
	}

	// A late reader from offset 0 gets only the retained tail: old events
	// were dropped, but the cursor stays at the true end.
	var out svcOutput
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/sessions/"+sid+"/output?offset=0", nil, &out); code != http.StatusOK {
		t.Fatalf("output from 0: status %d", code)
	}
	if out.NextOffset != total {
		t.Fatalf("re-poll next_offset: %d (want %d)", out.NextOffset, total)
	}
	sum := 0
	for _, e := range out.Events {
		sum += len(unb64(t, e.Data))
	}
	if sum == 0 || sum > 32768 { // cap plus at most one oversized chunk
		t.Fatalf("retained events: %d bytes (want capped tail)", sum)
	}
	// White-box: the offset floor rose — old events were really dropped.
	ses := svc.lookup(sbx).session(sid)
	ses.mu.Lock()
	floor := ses.truncLen
	ses.mu.Unlock()
	if floor <= 0 {
		t.Fatal("oldest events not truncated (offset floor never raised)")
	}
}

func TestServiceOutputCapEightMiB(t *testing.T) {
	if sessionOutputCap != 8<<20 {
		t.Fatalf("sessionOutputCap default: %d (want 8 MiB)", sessionOutputCap)
	}
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_8m")

	const total = 9 << 20 // 9 MiB against an 8 MiB cap
	sid := startSession(t, srv, sbx, "sh", "-c", "dd if=/dev/zero bs=65536 count=144 2>/dev/null")
	final, _ := pollUntilDone(t, srv, sbx, sid, 30*time.Second)
	if final.ExitCode != 0 {
		t.Fatalf("dd exit: %d", final.ExitCode)
	}

	var out svcOutput
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/sessions/"+sid+"/output?offset=0", nil, &out); code != http.StatusOK {
		t.Fatalf("output from 0: status %d", code)
	}
	if out.NextOffset != total {
		t.Fatalf("next_offset: %d (want %d)", out.NextOffset, total)
	}
	sum := 0
	for _, e := range out.Events {
		sum += len(unb64(t, e.Data))
	}
	if sum > 8<<20 {
		t.Fatalf("retained %d bytes over the 8 MiB cap", sum)
	}
}

// --- session TTL reaper ---------------------------------------------------------------

func TestServiceSessionTTLReaper(t *testing.T) {
	withSessionTTL(t, 30*time.Millisecond)
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_ttl")

	sid := startSession(t, srv, sbx, "sleep", "60")
	var out svcOutput
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/sessions/"+sid+"/output?offset=0", nil, &out); code != http.StatusOK {
		t.Fatalf("output before reap: status %d", code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/sessions/"+sid+"/output?offset=0", nil, &out)
		if code == http.StatusNotFound {
			break
		}
		if code != http.StatusOK {
			t.Fatalf("output during reap wait: status %d", code)
		}
		if time.Now().After(deadline) {
			t.Fatal("idle session not reaped within 3s")
		}
		// Poll slower than the TTL: touching the session resets its idle
		// clock, so the loop itself must not keep it alive.
		time.Sleep(150 * time.Millisecond)
	}

	// The sandbox itself survives reaping its idle session.
	if code, got := execSandbox(t, srv, sbx, []string{"echo", "alive"}); code != 0 || strings.TrimSpace(got) != "alive" {
		t.Fatalf("exec after reap: exit %d out %q", code, got)
	}
}

// --- destroy kills sessions -------------------------------------------------------------

func TestServiceDestroyKillsSessions(t *testing.T) {
	cfg, backend := newTestConfig(t)
	svc, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_dk")
	sid := startSession(t, srv, sbx, "sleep", "60")

	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbx, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy: status %d", code)
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/sessions/"+sid+"/output?offset=0", nil, nil); code != http.StatusNotFound {
		t.Fatalf("session output after destroy: status %d (want 404)", code)
	}
	live, err := backend.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("containers after destroy: %d", len(live))
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := svc.Close(); err != nil { // idempotent
		t.Fatalf("Close twice: %v", err)
	}
}

// --- error paths ------------------------------------------------------------------------

func TestServiceNotFound(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_nf")
	dead := "sbx_does_not_exist"

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/sandbox/" + dead + "/exec", map[string]any{"argv": []string{"true"}}},
		{http.MethodPost, "/sandbox/" + dead + "/start", map[string]any{"argv": []string{"true"}}},
		{http.MethodGet, "/sandbox/" + dead + "/read?path=/x", nil},
		{http.MethodPost, "/sandbox/" + dead + "/write", map[string]any{"path": "/x"}},
		{http.MethodGet, "/sandbox/" + dead + "/stat?path=/x", nil},
		{http.MethodPost, "/sandbox/" + dead + "/mkdir", map[string]any{"path": "/x"}},
		{http.MethodPost, "/sandbox/" + dead + "/glob", map[string]any{"pattern": "/x/*"}},
		{http.MethodPost, "/sandbox/" + dead + "/grep", map[string]any{"root": "/x", "pattern": "p"}},
		{http.MethodPost, "/sandbox/" + dead + "/workspace/sync", nil},
		{http.MethodGet, "/sandbox/" + dead + "/sessions/ses_1/output", nil},
		{http.MethodGet, "/sandbox/" + sbx + "/sessions/ses_missing/output", nil},
		{http.MethodPost, "/sandbox/" + sbx + "/sessions/ses_missing/input", map[string]any{"data": ""}},
		{http.MethodPost, "/sandbox/" + sbx + "/sessions/ses_missing/kill", nil},
	}
	for _, tc := range cases {
		if code := call(t, srv, tc.method, tc.path, tc.body, nil); code != http.StatusNotFound {
			t.Fatalf("%s %s: status %d (want 404)", tc.method, tc.path, code)
		}
	}
}

func TestServiceBadRequests(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_bad")
	sid := startSession(t, srv, sbx, "sleep", "30")

	// Malformed JSON bodies are 400s.
	for _, p := range []string{"/sandbox/create", "/sandbox/" + sbx + "/exec"} {
		if code, _, _ := callBytes(t, srv, http.MethodPost, p, "application/json", []byte("{not json"), ""); code != http.StatusBadRequest {
			t.Fatalf("POST %s with malformed JSON: status %d (want 400)", p, code)
		}
	}

	jsonCases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/sandbox/" + sbx + "/exec", map[string]any{"argv": []string{}}},
		{http.MethodPost, "/sandbox/" + sbx + "/exec", map[string]any{"argv": []string{"sh", "-c", "exit 0"}, "stdin": "!!!not base64!!!"}},
		{http.MethodPost, "/sandbox/" + sbx + "/start", map[string]any{"argv": []string{}}},
		{http.MethodPost, "/sandbox/" + sbx + "/write", map[string]any{"path": ""}},
		{http.MethodPost, "/sandbox/" + sbx + "/write", map[string]any{"path": "relative/x"}},
		{http.MethodPost, "/sandbox/" + sbx + "/write", map[string]any{"path": "/workspace/x", "content": "%%%"}},
		{http.MethodPost, "/sandbox/" + sbx + "/mkdir", map[string]any{"path": ""}},
		{http.MethodPost, "/sandbox/" + sbx + "/mkdir", map[string]any{"path": "relative"}},
		{http.MethodPost, "/sandbox/" + sbx + "/grep", map[string]any{"root": "/workspace", "pattern": "["}},
		{http.MethodPost, "/sandbox/" + sbx + "/sessions/" + sid + "/input", map[string]any{"data": "%%%"}},
	}
	for _, tc := range jsonCases {
		if code := call(t, srv, tc.method, tc.path, tc.body, nil); code != http.StatusBadRequest {
			t.Fatalf("%s %s %v: status %d (want 400)", tc.method, tc.path, tc.body, code)
		}
	}

	queryCases := []string{
		"/sandbox/" + sbx + "/read",
		"/sandbox/" + sbx + "/read?path=relative.txt",
		"/sandbox/" + sbx + "/stat",
		"/sandbox/" + sbx + "/sessions/" + sid + "/output?offset=abc",
	}
	for _, p := range queryCases {
		if code := call(t, srv, http.MethodGet, p, nil, nil); code != http.StatusBadRequest {
			t.Fatalf("GET %s: status %d (want 400)", p, code)
		}
	}

	// Missing files are 404, not 400.
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/read?path=/workspace/nope.txt", nil, nil); code != http.StatusNotFound {
		t.Fatalf("read missing: status %d", code)
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/stat?path=/workspace/nope.txt", nil, nil); code != http.StatusNotFound {
		t.Fatalf("stat missing: status %d", code)
	}
	// Wrong method on a registered route is the mux's 405.
	if code := call(t, srv, http.MethodGet, "/sandbox/create", nil, nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET create: status %d (want 405)", code)
	}
}

// --- archive endpoints -------------------------------------------------------------------

func TestServiceArchiveRoundtrip(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_arch")

	// Build a small tar.gz: a.txt + sub/b.txt.
	var tarbuf bytes.Buffer
	zw := gzip.NewWriter(&tarbuf)
	tw := tar.NewWriter(zw)
	for _, e := range []struct {
		name, data string
	}{
		{"a.txt", "alpha\n"},
		{"sub/b.txt", "beta\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	_ = tw.Close()
	_ = zw.Close()

	// 13. manual import → 204, content lands in /workspace — which is the
	// bind-mounted workspace directory, so the file is on the volume
	// directly (the import is the only direct volume write left).
	if code, _, _ := callBytes(t, srv, http.MethodPut, "/sandbox/"+sbx+"/archive", "application/gzip", tarbuf.Bytes(), ""); code != http.StatusNoContent {
		t.Fatalf("put archive: status %d", code)
	}
	if _, got := execSandbox(t, srv, sbx, []string{"cat", "a.txt"}); got != "alpha\n" {
		t.Fatalf("archive upload content: %q", got)
	}
	if host, err := cfg.Store.Dir("ws_arch"); err != nil {
		t.Fatalf("Store.Dir: %v", err)
	} else if b, err := os.ReadFile(host + "/a.txt"); err != nil || string(b) != "alpha\n" {
		t.Fatalf("manual import not on the volume: (%q, %v)", b, err)
	}

	// 14. download → gzip tar with both entries.
	code, body, hdr := callBytes(t, srv, http.MethodGet, "/sandbox/"+sbx+"/archive", "", nil, "")
	if code != http.StatusOK {
		t.Fatalf("get archive: status %d", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("archive content-type: %q", ct)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response not gzip: %v", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("tar entry read: %v", err)
			}
			files[h.Name] = string(data)
		}
	}
	if files["a.txt"] != "alpha\n" || files["sub/b.txt"] != "beta\n" {
		t.Fatalf("archive round-trip files: %v", files)
	}

	// Garbage uploads are a 400, not a 500.
	if code, _, _ := callBytes(t, srv, http.MethodPut, "/sandbox/"+sbx+"/archive", "application/gzip", []byte("definitely not a tar"), ""); code != http.StatusBadRequest {
		t.Fatalf("put garbage archive: status %d (want 400)", code)
	}

	// Channel checks on the manual import (spec §5.3): absolute entry
	// names and escaping symlinks are rejected with 400.
	evil := func(entries ...tar.Header) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		for _, h := range entries {
			hdr := h
			if err := tw.WriteHeader(&hdr); err != nil {
				t.Fatalf("tar header: %v", err)
			}
		}
		_ = tw.Close()
		_ = zw.Close()
		return buf.Bytes()
	}
	if code, _, _ := callBytes(t, srv, http.MethodPut, "/sandbox/"+sbx+"/archive", "application/gzip",
		evil(tar.Header{Name: "/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}), ""); code != http.StatusBadRequest {
		t.Fatalf("put archive with absolute entry: status %d (want 400)", code)
	}
	if code, _, _ := callBytes(t, srv, http.MethodPut, "/sandbox/"+sbx+"/archive", "application/gzip",
		evil(tar.Header{Name: "lnk", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}), ""); code != http.StatusBadRequest {
		t.Fatalf("put archive with escaping symlink: status %d (want 400)", code)
	}
}

// --- edge cases: config failures, injected backend failures, misc branches ---

// failingBackend injects per-method failures around a real Backend.
type failingBackend struct {
	Backend
	createErr error
	removeErr error
	startErr  error
}

type failingContainer struct {
	Container
	removeErr error
	startErr  error
}

func (b *failingBackend) Create(ctx context.Context, spec CreateSpec) (Container, error) {
	if b.createErr != nil {
		return nil, b.createErr
	}
	c, err := b.Backend.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &failingContainer{Container: c, removeErr: b.removeErr, startErr: b.startErr}, nil
}

func (c *failingContainer) Remove(ctx context.Context, force bool) error {
	if c.removeErr != nil {
		return c.removeErr
	}
	return c.Container.Remove(ctx, force)
}

func (c *failingContainer) Start(ctx context.Context, req ExecSpec) (Process, error) {
	if c.startErr != nil {
		return nil, c.startErr
	}
	return c.Container.Start(ctx, req)
}

func TestServiceConfigFailures(t *testing.T) {
	// Workspace routing without a store is an explicit 500.
	cfg, _ := newTestConfig(t)
	cfg.Store = nil
	_, srv := startTestService(t, cfg)
	if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_nostore", "cwd": "/workspace"}, nil); code != http.StatusInternalServerError {
		t.Fatalf("create without store: status %d (want 500)", code)
	}

	// A store whose root cannot be created fails create with 500.
	cfg2, _ := newTestConfig(t)
	rootFile := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(rootFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2.Store = NewWorkspaceStore(rootFile, 64<<20)
	_, srv2 := startTestService(t, cfg2)
	if code := call(t, srv2, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_badroot", "cwd": "/workspace"}, nil); code != http.StatusInternalServerError {
		t.Fatalf("create with broken store root: status %d (want 500)", code)
	}

	// Options that fail Normalize reject the create (and release the lock).
	cfg3, _ := newTestConfig(t)
	cfg3.Options = func() nacosrt.BackendOptions { return nacosrt.BackendOptions{Image: "i:1", Network: "host"} }
	_, srv3 := startTestService(t, cfg3)
	if code := call(t, srv3, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_opts", "cwd": "/workspace"}, nil); code != http.StatusInternalServerError {
		t.Fatalf("create with invalid options: status %d (want 500)", code)
	}
	dir, err := cfg3.Store.Dir("ws_opts")
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := LockDir(dir); err != nil {
		t.Fatalf("workspace lock after options failure: %v", err)
	} else {
		_ = lock.Close()
	}

	// A backend that refuses Create surfaces 500 with the lock released.
	cfg4, _ := newTestConfig(t)
	cfg4.Options = func() nacosrt.BackendOptions { return nacosrt.BackendOptions{} } // empty image
	_, srv4 := startTestService(t, cfg4)
	if code := call(t, srv4, http.MethodPost, "/sandbox/create", map[string]any{"workspace": "ws_nocreate", "cwd": "/workspace"}, nil); code != http.StatusInternalServerError {
		t.Fatalf("create with failing backend: status %d (want 500)", code)
	}

	// A create with neither workspace nor cwd defaults the workdir.
	cfg5, _ := newTestConfig(t)
	_, srv5 := startTestService(t, cfg5)
	if code := call(t, srv5, http.MethodPost, "/sandbox/create", map[string]any{}, nil); code != http.StatusOK {
		t.Fatalf("bare create: status %d", code)
	}
}

func TestServiceBackendFailures(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.Backend = &failingBackend{
		Backend:   cfg.Backend,
		removeErr: errors.New("remove boom"),
		startErr:  errors.New("start boom"),
	}
	_, srv := startTestService(t, cfg)

	// Start failure is a 500.
	sbx := createSandbox(t, srv, "ws_fail")
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/start", map[string]any{"argv": []string{"cat"}}, nil); code != http.StatusInternalServerError {
		t.Fatalf("start with failing backend: status %d (want 500)", code)
	}
	// Destroy reports the Remove failure as 500 but stays idempotent.
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbx, nil, nil); code != http.StatusInternalServerError {
		t.Fatalf("destroy with failing backend: status %d (want 500)", code)
	}
	if code := call(t, srv, http.MethodDelete, "/sandbox/"+sbx, nil, nil); code != http.StatusNoContent {
		t.Fatalf("destroy retry after failure: status %d (want 204)", code)
	}

	// Close surfaces teardown errors from containers still registered.
	svc2cfg, _ := newTestConfig(t)
	svc2cfg.Backend = &failingBackend{Backend: svc2cfg.Backend, removeErr: errors.New("remove boom")}
	svc2, srv2 := startTestService(t, svc2cfg)
	createSandbox(t, srv2, "ws_fail2")
	if err := svc2.Close(); err == nil {
		t.Fatal("Close with failing Remove: want error, got nil")
	}
}

func TestServiceFileSurfaceEdges(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_edge")

	// Reading a directory is a 400, not a content payload.
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/read?path=/workspace", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("read directory: status %d (want 400)", code)
	}

	// Write without mode lands the backend default (0644).
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{
		"path": "/workspace/m0.txt", "content": b64("modeless"),
	}, nil); code != http.StatusNoContent {
		t.Fatalf("write without mode: status %d", code)
	}
	var statOut struct {
		Mode  int64 `json:"mode"`
		IsDir bool  `json:"is_dir"`
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+sbx+"/stat?path=/workspace/m0.txt", nil, &statOut); code != http.StatusOK || statOut.Mode != 420 {
		t.Fatalf("stat default mode: code=%d mode=%d", code, statOut.Mode)
	}

	// Writing or mkdir-ing the root itself is a 400.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{"path": "/", "content": ""}, nil); code != http.StatusBadRequest {
		t.Fatalf("write root: status %d (want 400)", code)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/mkdir", map[string]any{"path": "/"}, nil); code != http.StatusBadRequest {
		t.Fatalf("mkdir root: status %d (want 400)", code)
	}

	// Glob: empty pattern, missing root, and a no-meta exact pattern.
	var globOut struct {
		Matches []string `json:"matches"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/glob", map[string]any{"pattern": ""}, &globOut); code != http.StatusOK || len(globOut.Matches) != 0 {
		t.Fatalf("glob empty pattern: code=%d matches=%v", code, globOut.Matches)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/glob", map[string]any{"pattern": "/workspace/nowhere/*"}, &globOut); code != http.StatusOK || len(globOut.Matches) != 0 {
		t.Fatalf("glob missing root: code=%d matches=%v", code, globOut.Matches)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/glob", map[string]any{"pattern": "/workspace/m0.txt"}, &globOut); code != http.StatusOK || len(globOut.Matches) != 1 || globOut.Matches[0] != "/workspace/m0.txt" {
		t.Fatalf("glob exact path: code=%d matches=%v", code, globOut.Matches)
	}

	// Grep: missing root 404, hidden dirs skipped, max_matches enforced,
	// default max applied.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/grep", map[string]any{"root": "/workspace/nowhere", "pattern": "x"}, nil); code != http.StatusNotFound {
		t.Fatalf("grep missing root: status %d (want 404)", code)
	}
	writeViaEndpoint(t, srv, sbx, "/workspace/.hidden/secret.txt", "secret")
	writeViaEndpoint(t, srv, sbx, "/workspace/one.txt", "capme one")
	writeViaEndpoint(t, srv, sbx, "/workspace/two.txt", "capme two")
	var grepOut struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/grep", map[string]any{"root": "/workspace", "pattern": "secret"}, &grepOut); code != http.StatusOK || len(grepOut.Matches) != 0 {
		t.Fatalf("grep hidden dir: code=%d matches=%d", code, len(grepOut.Matches))
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/grep", map[string]any{"root": "/workspace", "pattern": "capme", "max_matches": 1}, &grepOut); code != http.StatusOK || len(grepOut.Matches) != 1 {
		t.Fatalf("grep capped: code=%d matches=%d", code, len(grepOut.Matches))
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/grep", map[string]any{"root": "/workspace", "pattern": "capme"}, &grepOut); code != http.StatusOK || len(grepOut.Matches) != 2 {
		t.Fatalf("grep default cap: code=%d matches=%d", code, len(grepOut.Matches))
	}
}

func writeViaEndpoint(t *testing.T, srv *httptest.Server, sbx, p, content string) {
	t.Helper()
	if code := call(t, srv, http.MethodPost, "/sandbox/"+sbx+"/write", map[string]any{
		"path": p, "content": b64(content), "mode": 420,
	}, nil); code != http.StatusNoContent {
		t.Fatalf("write %s: status %d", p, code)
	}
}

func TestServiceMaxFileBytes(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.MaxFileBytes = 4096
	_, srv := startTestService(t, cfg)

	// A sandbox with only small files: every file endpoint works.
	small := createSandbox(t, srv, "ws_small_files")
	writeViaEndpoint(t, srv, small, "/workspace/small.txt", "tiny")
	var readOut struct {
		Content string `json:"content"`
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+small+"/read?path=/workspace/small.txt", nil, &readOut); code != http.StatusOK || unb64(t, readOut.Content) != "tiny" {
		t.Fatalf("read small: code=%d content=%q", code, unb64(t, readOut.Content))
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+small+"/stat?path=/workspace/small.txt", nil, nil); code != http.StatusOK {
		t.Fatalf("stat small: status %d", code)
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+small+"/grep", map[string]any{"root": "/workspace", "pattern": "tiny"}, nil); code != http.StatusOK {
		t.Fatalf("grep small: status %d", code)
	}

	// A sandbox carrying an 8 KiB file against the 4 KiB cap. The
	// refusal happens at the tar header's size field — before a single
	// byte of the entry is read — and the error body names the file and
	// the cap.
	big := createSandbox(t, srv, "ws_big_file")
	writeViaEndpoint(t, srv, big, "/workspace/huge.bin", strings.Repeat("z", 8192))
	var errOut struct {
		Error string `json:"error"`
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+big+"/read?path=/workspace/huge.bin", nil, &errOut); code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(errOut.Error, "4096") || !strings.Contains(errOut.Error, "huge.bin") {
		t.Fatalf("read huge: code=%d error=%q", code, errOut.Error)
	}
	if code := call(t, srv, http.MethodGet, "/sandbox/"+big+"/stat?path=/workspace/huge.bin", nil, &errOut); code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(errOut.Error, "4096") {
		t.Fatalf("stat huge: code=%d error=%q", code, errOut.Error)
	}
	// Grep walks the tree and refuses the over-cap file even when the
	// pattern would not have matched it.
	if code := call(t, srv, http.MethodPost, "/sandbox/"+big+"/grep", map[string]any{"root": "/workspace", "pattern": "zz"}, &errOut); code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(errOut.Error, "huge.bin") {
		t.Fatalf("grep huge: code=%d error=%q", code, errOut.Error)
	}

	// Glob only walks headers, so the capped file still lists fine.
	var globOut struct {
		Matches []string `json:"matches"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/"+big+"/glob", map[string]any{"pattern": "/workspace/*.bin"}, &globOut); code != http.StatusOK || len(globOut.Matches) != 1 {
		t.Fatalf("glob capped file: code=%d matches=%v", code, globOut.Matches)
	}

	// Zero config normalizes to the 8 MiB default.
	cfg2, _ := newTestConfig(t)
	svc2, _ := startTestService(t, cfg2)
	if svc2.cfg.MaxFileBytes != 8<<20 {
		t.Fatalf("default MaxFileBytes: %d (want %d)", svc2.cfg.MaxFileBytes, 8<<20)
	}
}

// A manual archive import past sandbox-archive-max-bytes answers 413 —
// the size cap moved from the deleted sync path onto the one remaining
// volume-write channel (spec §5.3).
func TestServiceArchiveImportOverCap(t *testing.T) {
	cfg, _ := newTestConfig(t)
	cfg.Store = NewWorkspaceStore(t.TempDir(), 4096)
	_, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_small")

	// Highly compressible payload: over the cap uncompressed, tiny as gzip.
	var tarbuf bytes.Buffer
	zw := gzip.NewWriter(&tarbuf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "big.txt", Mode: 0o644, Size: 64 << 10, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("y"), 64<<10)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = zw.Close()

	if code, _, _ := callBytes(t, srv, http.MethodPut, "/sandbox/"+sbx+"/archive", "application/gzip", tarbuf.Bytes(), ""); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("archive import over cap: status %d (want 413)", code)
	}
	// The workspace directory survives the rejected import untouched.
	dir, err := cfg.Store.Dir("ws_small")
	if err != nil || dir == "" {
		t.Fatalf("workspace dir after rejected import: %q err %v", dir, err)
	}
	if names := listDirNames(t, dir); len(names) != 0 {
		t.Fatalf("workspace dir disturbed by rejected import: %v", names)
	}
}

func TestServiceEmptyArchive(t *testing.T) {
	cfg, _ := newTestConfig(t)
	_, srv := startTestService(t, cfg)
	// cwd outside /workspace: the container has no /workspace to pack.
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	if code := call(t, srv, http.MethodPost, "/sandbox/create", map[string]any{"cwd": "/elsewhere"}, &out); code != http.StatusOK {
		t.Fatalf("create: status %d", code)
	}
	code, body, hdr := callBytes(t, srv, http.MethodGet, "/sandbox/"+out.SandboxID+"/archive", "", nil, "")
	if code != http.StatusOK || hdr.Get("Content-Type") != "application/gzip" {
		t.Fatalf("empty archive: status %d ct %q", code, hdr.Get("Content-Type"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("empty archive not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("empty archive has entries: %v", err)
	}
}

func TestServiceCloseAfterBackendGone(t *testing.T) {
	cfg, backend := newTestConfig(t)
	svc, srv := startTestService(t, cfg)
	createSandbox(t, srv, "ws_gone")

	// The backend tearing down first (containers hard-removed) must not
	// make Close fail: already-gone containers are a success, not an error.
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close after backend close: %v", err)
	}
}

func TestProcSessionCloseIdempotent(t *testing.T) {
	cfg, _ := newTestConfig(t)
	svc, srv := startTestService(t, cfg)
	sbx := createSandbox(t, srv, "ws_close")
	sid := startSession(t, srv, sbx, "sleep", "30")

	ses := svc.lookup(sbx).session(sid)
	ses.close()
	ses.close() // second close takes the early return
}
