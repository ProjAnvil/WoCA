package pluginrt

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/executor"
)

// TestExtractZipMemberOpenFails corrupts the LOCAL file header of a zip
// member while keeping the central directory intact: OpenReader succeeds but
// the member's Open fails, covering extractZipMembers' f.Open error branch.
func TestExtractZipMemberOpenFails(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("uv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	i := bytes.Index(data, []byte("PK\x03\x04"))
	if i < 0 {
		t.Fatal("no local header in test zip")
	}
	bad := append([]byte(nil), data...)
	copy(bad[i:], "XXXX")

	archive := filepath.Join(t.TempDir(), "uv.zip")
	if err := os.WriteFile(archive, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	err = extractArchiveMembers(archive, t.TempDir(), map[string]bool{"uv": true})
	if err == nil || !strings.Contains(err.Error(), "not a valid zip file") {
		t.Fatalf("extract from corrupt zip = %v, want member open error", err)
	}
}

// TestPythonRunStartFailsOnNonExecutableVenv: a Ready-stamped environment
// whose venv interpreter lost its execute bit makes the subprocess Start fail
// deterministically (fork/exec permission denied), covering Run's Start error
// return without touching the network.
func TestPythonRunStartFailsOnNonExecutableVenv(t *testing.T) {
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, py, "pdf", nil)
	if !py.Ready("pdf") {
		t.Fatal("stamped env should be Ready")
	}
	if err := os.Chmod(py.venvPython("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := py.Run(context.Background(), "pdf", "scripts/pdf.py", nil, ".", time.Second)
	if err == nil {
		t.Fatal("Run should fail when the venv interpreter is not executable")
	}
}

// failingRunUVExec answers the uv --version probe but fails every other
// Exec, so the shared-uv check passes and the first runUV start failure
// surfaces deterministically.
type failingRunUVExec struct{ executor.Executor }

func (f failingRunUVExec) Exec(ctx context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "--version" {
		return executor.ExecResult{Stdout: []byte("uv " + uvVersion)}, nil
	}
	return executor.ExecResult{}, fmt.Errorf("simulated start failure")
}

// TestPythonInstallRunUVStartFailure covers runUV's Exec error return: the
// version probe succeeds, then `uv python install` cannot start.
func TestPythonInstallRunUVStartFailure(t *testing.T) {
	py := NewPythonRuntimeWithExecutor(t.TempDir(), t.TempDir(), zerolog.Nop(), failingRunUVExec{executor.NewLocal()})
	if err := os.MkdirAll(filepath.Dir(py.uvBin()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py.uvBin(), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
	if err == nil || !strings.Contains(err.Error(), "uv python install") {
		t.Fatalf("err = %v, want uv python install failure", err)
	}
}
