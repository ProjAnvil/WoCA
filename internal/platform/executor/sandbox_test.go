package executor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeSandboxServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(v))
	}
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"sandbox_id":"sbx_test"}`)
	})
	mux.HandleFunc("POST /sandbox/sbx_test/exec", func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		// contract v2: base64 payloads ("sandbox output\n")
		write(w, `{"exit_code":0,"stdout":"c2FuZGJveCBvdXRwdXQK","stderr":""}`)
	})
	mux.HandleFunc("GET /sandbox/sbx_test/read", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"content":"aGVsbG8="}`) // base64 "hello"
	})
	mux.HandleFunc("POST /sandbox/sbx_test/write", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_test/glob", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"matches":["/a.go","/b.go"]}`)
	})
	mux.HandleFunc("DELETE /sandbox/sbx_test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

func TestSandboxExecRunsCommand(t *testing.T) {
	srv := newFakeSandboxServer(t)
	defer srv.Close()
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL, APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(t.Context(), ExecRequest{Name: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "sandbox output") {
		t.Fatalf("res = %+v", res)
	}
}

func TestSandboxReadWriteFile(t *testing.T) {
	srv := newFakeSandboxServer(t)
	defer srv.Close()
	sb, _ := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	defer sb.Close()

	data, err := sb.ReadFile(t.Context(), "/x.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read = %q err=%v", string(data), err)
	}
	if err := sb.WriteFile(t.Context(), "/x.txt", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxGlob(t *testing.T) {
	srv := newFakeSandboxServer(t)
	defer srv.Close()
	sb, _ := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	defer sb.Close()

	matches, err := sb.Glob(t.Context(), "*.go")
	if err != nil || len(matches) != 2 {
		t.Fatalf("glob = %v err=%v", matches, err)
	}
}

func TestSandboxUnimplementedSurfacesError(t *testing.T) {
	srv := newFakeSandboxServer(t)
	defer srv.Close()
	sb, _ := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	defer sb.Close()

	// Start/MkdirAll/Stat/Grep are v2 — calling them surfaces explicit
	// errors, not panics or silent successes.
	if _, err := sb.Start(t.Context(), ExecRequest{}, StartOptions{}); err == nil {
		t.Fatal("Start on sandbox succeeded")
	}
	if err := sb.MkdirAll(t.Context(), "/x", 0o755); err == nil {
		t.Fatal("MkdirAll on sandbox succeeded")
	}
	if _, err := sb.Stat(t.Context(), "/x"); err == nil {
		t.Fatal("Stat on sandbox succeeded")
	}
	if _, err := sb.Grep(t.Context(), "/", nil); err == nil {
		t.Fatal("Grep on sandbox succeeded")
	}
}

func TestSandboxConfigValidation(t *testing.T) {
	if _, err := NewSandbox(SandboxConfig{}); err == nil {
		t.Fatal("empty endpoint accepted")
	}
	if _, err := NewSandbox(SandboxConfig{Endpoint: "bad://"}); err == nil {
		t.Fatal("malformed endpoint accepted")
	}
}

func TestSandboxCloseDestroys(t *testing.T) {
	var destroyed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandbox/create" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandbox_id":"sbx_close"}`))
			return
		}
		if r.Method == http.MethodDelete {
			destroyed = true
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	sb, _ := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	if _, err := sb.Exec(t.Context(), ExecRequest{Name: "true"}); err != nil {
		t.Fatal(err) // create was faked by the catch-all handler
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if !destroyed {
		t.Fatal("Close did not destroy the sandbox")
	}
}

var _ = bytes.MinRead
