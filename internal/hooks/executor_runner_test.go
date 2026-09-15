package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
)

// recordingExecutor captures Exec requests and replays a canned result.
type recordingExecutor struct {
	reqs   []executor.ExecRequest
	result executor.ExecResult
	err    error
}

func (e *recordingExecutor) Exec(_ context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	e.reqs = append(e.reqs, req)
	return e.result, e.err
}

func (e *recordingExecutor) Start(context.Context, executor.ExecRequest, executor.StartOptions) (executor.Process, error) {
	return nil, errors.New("recordingExecutor: Start not expected in this test")
}

func (e *recordingExecutor) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (e *recordingExecutor) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return nil
}
func (e *recordingExecutor) MkdirAll(context.Context, string, fs.FileMode) error { return nil }
func (e *recordingExecutor) Stat(context.Context, string) (fs.FileInfo, error)   { return nil, nil }
func (e *recordingExecutor) Glob(context.Context, string) ([]string, error)      { return nil, nil }
func (e *recordingExecutor) Grep(context.Context, string, *regexp.Regexp) ([]executor.GrepMatch, error) {
	return nil, nil
}

func TestExecutorCommandRunnerRoutesThroughExecutor(t *testing.T) {
	fake := &recordingExecutor{result: executor.ExecResult{
		Stdout: []byte("ok\n"),
		Stderr: []byte("careful\n"),
	}}
	cfg := Config{PreToolUse: []Entry{{Hooks: []Hook{{Type: "command", Command: "policy check"}}}}}
	r := NewRunner(cfg, "/work/dir", "sess-1", WithCommandRunner(ExecutorCommandRunner(fake)))

	d, err := r.PreToolUse(t.Context(), "Bash", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	if d.Feedback != "ok" || d.Warning != "careful" {
		t.Fatalf("feedback/warning = %q/%q", d.Feedback, d.Warning)
	}

	if len(fake.reqs) != 1 {
		t.Fatalf("executor saw %d requests, want 1", len(fake.reqs))
	}
	req := fake.reqs[0]
	if req.Name != "sh" || len(req.Args) != 2 || req.Args[0] != "-c" || req.Args[1] != "policy check" {
		t.Fatalf("request = %+v", req)
	}
	if req.Dir != "/work/dir" {
		t.Fatalf("dir = %q", req.Dir)
	}
	var ev Event
	if err := json.Unmarshal(req.Stdin, &ev); err != nil {
		t.Fatalf("stdin is not the event payload: %v", err)
	}
	if ev.SessionID != "sess-1" || ev.ToolName != "Bash" || ev.CWD != "/work/dir" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestExecutorCommandRunnerDenyPassthrough(t *testing.T) {
	fake := &recordingExecutor{result: executor.ExecResult{
		Stdout:   []byte("blocked by policy\n"),
		ExitCode: 2,
	}}
	cfg := Config{PreToolUse: []Entry{{Hooks: []Hook{{Type: "command", Command: "deny"}}}}}
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(ExecutorCommandRunner(fake)))

	d, err := r.PreToolUse(t.Context(), "Bash", map[string]any{"command": "rm"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("expected deny on exit code 2")
	}
	if !strings.Contains(d.Feedback, "blocked by policy") {
		t.Fatalf("feedback = %q", d.Feedback)
	}
}

func TestExecutorCommandRunnerErrorPassthrough(t *testing.T) {
	fake := &recordingExecutor{err: errors.New("spawn failed")}
	run := ExecutorCommandRunner(fake)
	if _, err := run(t.Context(), "cmd", "stdin", "/tmp"); err == nil || !strings.Contains(err.Error(), "spawn failed") {
		t.Fatalf("err = %v", err)
	}
}
