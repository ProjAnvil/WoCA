package executor

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// wireCapture records the raw HTTP interactions issued by Sandbox so tests
// can assert the contract v2 wire shape (CONTRACT.md).
type wireCapture struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   json.RawMessage
}

func (c *wireCapture) record(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
}

func (c *wireCapture) find(method, path string) *capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.reqs {
		if c.reqs[i].Method == method && c.reqs[i].Path == path {
			return &c.reqs[i]
		}
	}
	return nil
}

// all returns a copy of every captured request in issue order.
func (c *wireCapture) all() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reqs)
}

// handler serves contract v2 well-formed responses while recording requests.
func (c *wireCapture) handler() http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, v string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(v))
	}
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		c.record(r, body)
		respond(w, `{"sandbox_id":"sbx_wire"}`)
	})
	mux.HandleFunc("POST /sandbox/sbx_wire/exec", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		c.record(r, body)
		respond(w, `{"exit_code":0,"stdout":"aGkK","stderr":""}`)
	})
	mux.HandleFunc("GET /sandbox/sbx_wire/read", func(w http.ResponseWriter, r *http.Request) {
		c.record(r, nil)
		respond(w, `{"content":"aGk="}`)
	})
	mux.HandleFunc("POST /sandbox/sbx_wire/write", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		c.record(r, body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_wire/glob", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		c.record(r, body)
		respond(w, `{"matches":[]}`)
	})
	mux.HandleFunc("PUT /sandbox/sbx_wire/archive", func(w http.ResponseWriter, r *http.Request) {
		c.record(r, nil)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_wire/workspace/sync", func(w http.ResponseWriter, r *http.Request) {
		c.record(r, nil)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /sandbox/sbx_wire", func(w http.ResponseWriter, r *http.Request) {
		c.record(r, nil)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readAll(r *http.Request) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func newWireSandbox(t *testing.T) (*Sandbox, *wireCapture) {
	t.Helper()
	c := &wireCapture{}
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL, APIKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb, c
}

// newWireWorkspaceSandbox is newWireSandbox with Workspace set, so wire
// tests can assert the workspace field and routing header (CONTRACT.md §1,
// §3.1, §3.16).
func newWireWorkspaceSandbox(t *testing.T) (*Sandbox, *wireCapture) {
	t.Helper()
	c := &wireCapture{}
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL, APIKey: "k1", Workspace: "ws_x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb, c
}

func TestSandboxWireContractHeader(t *testing.T) {
	sb, c := newWireSandbox(t)
	if _, err := sb.Exec(t.Context(), ExecRequest{Name: "echo", Args: []string{"hi"}}); err != nil {
		t.Fatal(err)
	}
	req := c.find(http.MethodPost, "/sandbox/sbx_wire/exec")
	if req == nil {
		t.Fatal("exec request not captured")
	}
	if got := req.Header.Get("X-Woca-Sandbox-Contract"); got != "2" {
		t.Fatalf("contract header = %q, want 2", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer k1" {
		t.Fatalf("authorization = %q, want Bearer k1", got)
	}
}

func TestSandboxWireCreateBody(t *testing.T) {
	_, c := newWireSandbox(t)
	req := c.find(http.MethodPost, "/sandbox/create")
	if req == nil {
		t.Fatal("create request not captured")
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("create body not JSON: %v", err)
	}
	if body["cwd"] != "/workspace" {
		t.Fatalf("create body cwd = %v, want /workspace", body["cwd"])
	}
}

// TestSandboxWireWorkspaceCreateBody asserts the contract v2 workspace
// amendment: create carries the workspace ID as a dedicated field next to
// cwd (which keeps its container-working-directory meaning).
func TestSandboxWireWorkspaceCreateBody(t *testing.T) {
	_, c := newWireWorkspaceSandbox(t)
	req := c.find(http.MethodPost, "/sandbox/create")
	if req == nil {
		t.Fatal("create request not captured")
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("create body not JSON: %v", err)
	}
	if body["workspace"] != "ws_x" {
		t.Fatalf("create body workspace = %v, want ws_x", body["workspace"])
	}
	if body["cwd"] != "/workspace" {
		t.Fatalf("create body cwd = %v, want /workspace", body["cwd"])
	}
}

// TestSandboxWireWorkspaceHeaderEveryRequest asserts the routing header is
// stamped on every sandbox request — JSON calls, raw archive calls, sync,
// and destroy — when the sandbox is workspace-routed.
func TestSandboxWireWorkspaceHeaderEveryRequest(t *testing.T) {
	sb, c := newWireWorkspaceSandbox(t)
	if _, err := sb.Exec(t.Context(), ExecRequest{Name: "echo", Args: []string{"hi"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile(t.Context(), "/workspace/a.go"); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(t.Context(), "/workspace/a.go", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Glob(t.Context(), "*.go"); err != nil {
		t.Fatal(err)
	}
	if err := sb.PutArchive(t.Context(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if err := sb.SyncWorkspace(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	reqs := c.all()
	if len(reqs) < 7 {
		t.Fatalf("captured %d requests, want at least 7 (create/exec/read/write/glob/archive/sync/destroy)", len(reqs))
	}
	for _, req := range reqs {
		if got := req.Header.Get("X-Woca-Workspace"); got != "ws_x" {
			t.Fatalf("%s %s: X-Woca-Workspace = %q, want ws_x", req.Method, req.Path, got)
		}
	}
}

// TestSandboxWireWorkspaceHeaderOmittedWhenUnset: the routing header is
// optional — sandboxes that are not workspace-routed (personal runs) send
// nothing, and servers must not require it.
func TestSandboxWireWorkspaceHeaderOmittedWhenUnset(t *testing.T) {
	sb, c := newWireSandbox(t)
	if _, err := sb.Exec(t.Context(), ExecRequest{Name: "echo"}); err != nil {
		t.Fatal(err)
	}
	req := c.find(http.MethodPost, "/sandbox/sbx_wire/exec")
	if req == nil {
		t.Fatal("exec request not captured")
	}
	if got := req.Header.Get("X-Woca-Workspace"); got != "" {
		t.Fatalf("X-Woca-Workspace = %q, want omitted", got)
	}
}

// TestSandboxWireSyncWorkspace asserts the sync endpoint shape: POST
// /sandbox/{id}/workspace/sync with a 2xx response and no error.
func TestSandboxWireSyncWorkspace(t *testing.T) {
	sb, c := newWireWorkspaceSandbox(t)
	if err := sb.SyncWorkspace(t.Context()); err != nil {
		t.Fatalf("SyncWorkspace: %v", err)
	}
	req := c.find(http.MethodPost, "/sandbox/sbx_wire/workspace/sync")
	if req == nil {
		t.Fatal("sync request not captured")
	}
}

// TestSandboxWireSyncWorkspaceError: non-2xx from sync is an error.
func TestSandboxWireSyncWorkspaceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandbox/create" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandbox_id":"sbx_err"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"sync failed"}`))
	}))
	defer srv.Close()
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL, Workspace: "ws_x"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	if err := sb.SyncWorkspace(t.Context()); err == nil {
		t.Fatal("sync non-2xx not surfaced as error")
	} else if !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("sync error = %v, want http 500", err)
	}
}

func TestSandboxWireExecBody(t *testing.T) {
	sb, c := newWireSandbox(t)
	if _, err := sb.Exec(t.Context(), ExecRequest{
		Name:  "echo",
		Args:  []string{"a b", "c"},
		Dir:   "/workspace",
		Env:   []string{"FOO=bar", "EMPTY="},
		Stdin: []byte("line1\n"),
	}); err != nil {
		t.Fatal(err)
	}
	req := c.find(http.MethodPost, "/sandbox/sbx_wire/exec")
	if req == nil {
		t.Fatal("exec request not captured")
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("exec body not JSON: %v", err)
	}
	argv, ok := body["argv"].([]any)
	if !ok || len(argv) != 3 || argv[0] != "echo" || argv[1] != "a b" || argv[2] != "c" {
		t.Fatalf("argv = %v, want [echo a\\ b c]", body["argv"])
	}
	if body["cwd"] != "/workspace" {
		t.Fatalf("cwd = %v, want /workspace", body["cwd"])
	}
	env, ok := body["env"].(map[string]any)
	if !ok || env["FOO"] != "bar" || env["EMPTY"] != "" {
		t.Fatalf("env = %v, want {FOO:bar, EMPTY:}", body["env"])
	}
	if got, want := body["stdin"], base64.StdEncoding.EncodeToString([]byte("line1\n")); got != want {
		t.Fatalf("stdin = %v, want %q", got, want)
	}
	if _, ok := body["timeout_ms"]; !ok {
		t.Fatal("timeout_ms missing from exec body")
	}
	if _, ok := body["command"]; ok {
		t.Fatal("v1 command field still present")
	}
}

func TestSandboxWireExecDecodesBase64(t *testing.T) {
	sb, _ := newWireSandbox(t)
	res, err := sb.Exec(t.Context(), ExecRequest{Name: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "hi\n" {
		t.Fatalf("stdout = %q, want %q (base64-decoded)", res.Stdout, "hi\n")
	}
}

func TestSandboxWireWriteMode(t *testing.T) {
	sb, c := newWireSandbox(t)
	if err := sb.WriteFile(t.Context(), "/workspace/x.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := c.find(http.MethodPost, "/sandbox/sbx_wire/write")
	if req == nil {
		t.Fatal("write request not captured")
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("write body not JSON: %v", err)
	}
	if body["path"] != "/workspace/x.sh" {
		t.Fatalf("path = %v", body["path"])
	}
	if got, want := body["content"], base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\n")); got != want {
		t.Fatalf("content = %v, want %q", got, want)
	}
	if mode, ok := body["mode"].(float64); !ok || int(mode) != 0o755 {
		t.Fatalf("mode = %v, want %d", body["mode"], 0o755)
	}
}
