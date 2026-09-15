// Phase 3 T15 acceptance: sandbox execution with zero host mounts — the
// sandbox Executor operates entirely through HTTP (no local filesystem
// access for command execution, file reads, or writes); a host-side probe
// file written through the sandbox is not visible on the local filesystem.
package executor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A command executed through the sandbox goes to the remote API — the host
// filesystem is never touched (zero host mounts). The proof: write a file
// via the sandbox WriteFile and verify it does not exist on the local
// filesystem at the same path.
func TestSandboxZeroHostMounts(t *testing.T) {
	localDir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandbox_id":"sbx_zero"}`))
	})
	mux.HandleFunc("POST /sandbox/sbx_zero/write", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_zero/exec", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"exit_code":0,"stdout":"","stderr":""}`))
	})
	mux.HandleFunc("DELETE /sandbox/sbx_zero", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	// Write a file through the sandbox — it goes to the remote API, not
	// the local filesystem.
	sandboxPath := filepath.Join(localDir, "sandbox_only.txt")
	if err := sb.WriteFile(t.Context(), sandboxPath, []byte("sandbox data"), 0o644); err != nil {
		t.Fatalf("sandbox write: %v", err)
	}

	// The file must NOT exist on the host filesystem.
	if _, err := os.Stat(sandboxPath); err == nil {
		t.Fatal("sandbox write leaked to the host filesystem")
	}

	// Exec a command through the sandbox — no local process is spawned.
	if _, err := sb.Exec(t.Context(), ExecRequest{Name: "touch", Args: []string{filepath.Join(localDir, "touched.txt")}}); err != nil {
		t.Fatalf("sandbox exec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "touched.txt")); err == nil {
		t.Fatal("sandbox exec created a file on the host filesystem")
	}

	// The host directory is empty — nothing leaked.
	entries, _ := os.ReadDir(localDir)
	if len(entries) != 0 {
		t.Fatalf("host dir has %d entries after sandbox operations: %v", len(entries), entries)
	}
}
