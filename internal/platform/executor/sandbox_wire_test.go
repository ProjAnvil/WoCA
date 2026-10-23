package executor

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
