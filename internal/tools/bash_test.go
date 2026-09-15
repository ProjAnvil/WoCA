package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashToolWorkdirAndStreams(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	tool, _ := reg.Get("Bash")

	// Relative workdir is resolved against the session workdir.
	res, err := tool.Invoke(t.Context(), map[string]any{"command": "pwd", "workdir": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, filepath.Join(dir, "sub")) {
		t.Fatalf("relative workdir pwd = %q", res.Content)
	}
	// Absolute workdir is used verbatim.
	res, err = tool.Invoke(t.Context(), map[string]any{"command": "pwd", "workdir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, dir) {
		t.Fatalf("absolute workdir pwd = %q", res.Content)
	}

	// stderr is appended after stdout; failing commands report the exit code.
	res, err = tool.Invoke(t.Context(), map[string]any{"command": "echo out; echo err 1>&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Exit code 3") || !strings.Contains(res.Content, "out") || !strings.Contains(res.Content, "err") {
		t.Fatalf("streams+exit = %q", res.Content)
	}
	// stdout before stderr with the separating newline.
	if strings.Index(res.Content, "out") > strings.Index(res.Content, "err") {
		t.Fatalf("stderr not appended after stdout: %q", res.Content)
	}

	// Successful silent commands report a placeholder.
	res, err = tool.Invoke(t.Context(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "(no output)" {
		t.Fatalf("silent command = %q", res.Content)
	}
}

func TestBashToolTimeout(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("Bash")
	start := time.Now()
	res, err := tool.Invoke(t.Context(), map[string]any{"command": "sleep 5", "timeout_ms": 150})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "timed out after") {
		t.Fatalf("timeout result = %q", res.Content)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s to fire", elapsed)
	}
}

func TestBashBackgroundStartFailure(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("Bash")
	// The background runner cannot chdir into a nonexistent workdir.
	res, err := tool.Invoke(t.Context(), map[string]any{
		"command": "echo x", "workdir": "does-not-exist", "run_in_background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Error: start background command") {
		t.Fatalf("background start failure = %q", res.Content)
	}
}

func TestBashOutputWithOutputAndKillShellEdges(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	poll, _ := reg.Get("BashOutput")
	kill, _ := reg.Get("KillShell")

	// Error listing on a manager with no shells yet renders "none".
	res, _ := poll.Invoke(t.Context(), map[string]any{"shell_id": "404"})
	if !strings.Contains(res.Content, "Error") || !strings.Contains(res.Content, "(started shells: none)") {
		t.Fatalf("no shells = %q", res.Content)
	}

	bg := reg.Background()
	// printf without a newline exercises the newline-appending branch of
	// BashOutput's rendering.
	if _, err := bg.Start("printf partial-output; sleep 0.2", "", "."); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, _ = poll.Invoke(t.Context(), map[string]any{"shell_id": "1"})
		if strings.Contains(res.Content, "partial-output") && strings.Contains(res.Content, "done (exit code 0)") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(res.Content, "partial-output\n") {
		t.Fatalf("BashOutput never showed output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "shell 1: done (exit code 0)") {
		t.Fatalf("final BashOutput = %q", res.Content)
	}

	// Killing an unknown shell errors; killing a finished one is a no-op.
	res, _ = kill.Invoke(t.Context(), map[string]any{"shell_id": "404"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("KillShell unknown = %q", res.Content)
	}
	res, _ = kill.Invoke(t.Context(), map[string]any{"shell_id": "1"})
	if !strings.Contains(res.Content, "already finished") {
		t.Fatalf("KillShell finished = %q", res.Content)
	}
}
