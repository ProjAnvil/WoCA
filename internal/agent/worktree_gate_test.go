package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/runexec"
	"github.com/projanvil/woca/internal/tools"
)

// EnterWorktree creates host-side git state (a worktree directory plus a
// branch) — meaningless and unreachable during a sandboxed run, so the
// tool refuses up front (design §10.1 C-2).
func TestEnterWorktreeSandboxGate(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{registry: tools.NewRegistry(dir)}
	enter := a.worktreeTools()[0]

	sbx, err := executor.NewSandbox(executor.SandboxConfig{Endpoint: fakesandbox.Addr(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sbx.Close() }()

	ctx := runexec.WithRunExecutor(context.Background(), sbx)
	res, err := enter.Invoke(ctx, map[string]any{"name": "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "not available during sandboxed runs") {
		t.Fatalf("sandboxed EnterWorktree = %q", res.Content)
	}

	// Local ctx (no injection): the gate stays open — the tool proceeds
	// to its regular "not a git repository" reply.
	res, err = enter.Invoke(context.Background(), map[string]any{"name": "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "sandboxed runs") {
		t.Fatalf("local EnterWorktree wrongly gated: %q", res.Content)
	}
}
