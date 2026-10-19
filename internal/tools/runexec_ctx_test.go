package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/runexec"
)

// The run-executor override (runexec.WithRunExecutor): when the invoking
// context carries one, every executor-backed tool must operate on it —
// the per-run sandbox (design §10.1 C-3) — instead of the build-time
// executor the registry was constructed with.

func TestRegistryExecForPrefersRunExecutor(t *testing.T) {
	build := &fakeExecutor{}
	run := &fakeExecutor{}
	reg := NewRegistryWithExecutor(t.TempDir(), build)

	if got := reg.ExecFor(context.Background()); got != executor.Executor(build) {
		t.Fatalf("ExecFor without injection: got %T, want the build-time executor", got)
	}

	ctx := runexec.WithRunExecutor(context.Background(), run)
	if got := reg.ExecFor(ctx); got != executor.Executor(run) {
		t.Fatalf("ExecFor with injection: got %T, want the run executor", got)
	}
}

func TestRegistryExecForFn(t *testing.T) {
	build := &fakeExecutor{}
	run := &fakeExecutor{}
	reg := NewRegistryWithExecutor(t.TempDir(), build)
	execFor := reg.ExecForFn()

	if execFor(context.Background()) != executor.Executor(build) {
		t.Fatal("ExecForFn without injection must return the build executor")
	}
	ctx := runexec.WithRunExecutor(context.Background(), run)
	if execFor(ctx) != executor.Executor(run) {
		t.Fatal("ExecForFn with injection must return the run executor")
	}
}

func TestBashToolUsesRunExecutorFromContext(t *testing.T) {
	dir := t.TempDir()
	build := &fakeExecutor{}
	run := &fakeExecutor{execRes: executor.ExecResult{Stdout: []byte("from-run\n")}}
	reg := NewRegistryWithExecutor(dir, build)
	tool, _ := reg.Get("Bash")

	ctx := runexec.WithRunExecutor(t.Context(), run)
	res, err := tool.Invoke(ctx, map[string]any{"command": "echo from-run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.execs) != 1 {
		t.Fatalf("run executor saw %d execs, want 1", len(run.execs))
	}
	if len(build.execs) != 0 {
		t.Fatalf("build executor saw %d execs, want 0", len(build.execs))
	}
	if req := run.execs[0]; req.Dir != dir {
		t.Fatalf("dir = %q, want %q", req.Dir, dir)
	}
	if res.Content != "from-run\n" {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestFileToolsUseRunExecutorFromContext(t *testing.T) {
	dir := t.TempDir()
	wantPath := filepath.Join(dir, "f.txt")
	build := &fakeExecutor{}
	run := &fakeExecutor{reads: map[string][]byte{wantPath: []byte("alpha\n")}}
	reg := NewRegistryWithExecutor(dir, build)
	ctx := runexec.WithRunExecutor(t.Context(), run)

	readTool, _ := reg.Get("Read")
	if _, err := readTool.Invoke(ctx, map[string]any{"file_path": "f.txt"}); err != nil {
		t.Fatal(err)
	}
	if len(build.reads) != 0 {
		t.Fatalf("build executor saw reads, want 0")
	}

	writeTool, _ := reg.Get("Write")
	if _, err := writeTool.Invoke(ctx, map[string]any{"file_path": "g.txt", "content": "x"}); err != nil {
		t.Fatal(err)
	}
	if len(build.writes) != 0 {
		t.Fatalf("build executor saw writes, want 0")
	}
	if len(run.writes) != 1 || run.writes[0].path != filepath.Join(dir, "g.txt") {
		t.Fatalf("run executor writes = %+v", run.writes)
	}

	editTool, _ := reg.Get("Edit")
	if _, err := editTool.Invoke(ctx, map[string]any{"file_path": "f.txt", "old_string": "alpha", "new_string": "beta"}); err != nil {
		t.Fatal(err)
	}
	if len(build.writes) != 0 {
		t.Fatalf("build executor saw edits, want 0")
	}
}

// A registry built the way newSubagent builds sub-registries
// (NewRegistryWithExecutor) resolves the run executor from the context
// too — the parent run's ctx flows into subagent tool invocations, so
// sandboxed runs stay sandboxed in Task-spawned subagents.
func TestSubregistryResolvesRunExecutor(t *testing.T) {
	dir := t.TempDir()
	run := &fakeExecutor{execRes: executor.ExecResult{Stdout: []byte("ok\n")}}
	// Built with the local executor as fallback, like newSubagent does.
	sub := NewRegistryWithExecutor(dir, nil)
	tool, _ := sub.Get("Bash")

	ctx := runexec.WithRunExecutor(t.Context(), run)
	if _, err := tool.Invoke(ctx, map[string]any{"command": "echo ok"}); err != nil {
		t.Fatal(err)
	}
	if len(run.execs) != 1 {
		t.Fatalf("run executor saw %d execs, want 1", len(run.execs))
	}
}
