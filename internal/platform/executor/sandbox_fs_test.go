package executor

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// fsFake serves the contract v2 filesystem endpoints (stat/mkdir/grep)
// from canned responses.
func newFsFakeSandbox(t *testing.T) *Sandbox {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(v))
	}
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"sandbox_id":"sbx_fs"}`)
	})
	mux.HandleFunc("GET /sandbox/sbx_fs/stat", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("path") {
		case "/workspace/pkg":
			write(w, `{"mode":493,"size":0,"mtime_ms":1700000000000,"is_dir":true}`)
		case "/workspace/main.go":
			write(w, `{"mode":420,"size":12,"mtime_ms":1700000000123,"is_dir":false}`)
		default:
			http.Error(w, `{"error":"not found"}`, 404)
		}
	})
	mux.HandleFunc("POST /sandbox/sbx_fs/mkdir", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_fs/grep", func(w http.ResponseWriter, _ *http.Request) {
		// one stdout-ish match with binary-safe base64 text ("TODO: fix" = VE9ETzogZml4)
		write(w, `{"matches":[{"path":"/workspace/a.go","line":3,"text":"VE9ETzogZml4"}]}`)
	})
	mux.HandleFunc("DELETE /sandbox/sbx_fs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func TestSandboxStatDir(t *testing.T) {
	sb := newFsFakeSandbox(t)
	fi, err := sb.Stat(t.Context(), "/workspace/pkg")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatal("IsDir = false, want true")
	}
	if fi.Name() != "pkg" {
		t.Fatalf("Name = %q, want pkg", fi.Name())
	}
	if fi.Size() != 0 {
		t.Fatalf("Size = %d, want 0", fi.Size())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("Mode perm = %o, want 755", fi.Mode().Perm())
	}
	if fi.ModTime().UnixMilli() != 1700000000000 {
		t.Fatalf("ModTime = %v", fi.ModTime())
	}
	if fi.Sys() != nil {
		t.Fatalf("Sys = %v, want nil", fi.Sys())
	}
}

func TestSandboxStatFile(t *testing.T) {
	sb := newFsFakeSandbox(t)
	fi, err := sb.Stat(t.Context(), "/workspace/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Fatal("IsDir = true, want false")
	}
	if fi.Name() != "main.go" {
		t.Fatalf("Name = %q, want main.go", fi.Name())
	}
	if fi.Size() != 12 {
		t.Fatalf("Size = %d, want 12", fi.Size())
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("Mode perm = %o, want 644", fi.Mode().Perm())
	}
	if fi.ModTime().UnixMilli() != 1700000000123 {
		t.Fatalf("ModTime = %v", fi.ModTime())
	}
}

func TestSandboxStatNotFound(t *testing.T) {
	sb := newFsFakeSandbox(t)
	if _, err := sb.Stat(t.Context(), "/workspace/missing"); err == nil {
		t.Fatal("stat of missing path succeeded")
	}
}

func TestSandboxMkdirAll(t *testing.T) {
	sb := newFsFakeSandbox(t)
	if err := sb.MkdirAll(t.Context(), "/workspace/pkg/sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
}

func TestSandboxGrep(t *testing.T) {
	sb := newFsFakeSandbox(t)
	matches, err := sb.Grep(t.Context(), "/workspace", regexp.MustCompile(`TODO`))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %v", matches)
	}
	m := matches[0]
	if m.Path != "/workspace/a.go" || m.Line != 3 || m.Text != "TODO: fix" {
		t.Fatalf("match = %+v", m)
	}
}
