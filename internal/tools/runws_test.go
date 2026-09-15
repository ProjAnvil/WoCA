package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/runws"
)

// The run-scoped workspace (serve-mode sessions) must scope the file/shell
// tools: one shared registry stays bound to the startup directory for
// fallback, while a run decorated with another workspace resolves relative
// paths there instead.

func TestBashRunWorkspaceScopesCwd(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	reg := NewRegistry(base)

	res := reg.Execute(runws.With(t.Context(), ws), "Bash", map[string]any{"command": "pwd"})
	if !strings.Contains(res.Content, ws) {
		t.Fatalf("pwd = %q, want it inside %q", res.Content, ws)
	}
	if strings.Contains(res.Content, base) && base != ws {
		t.Fatalf("pwd = %q leaked the base dir %q", res.Content, base)
	}
}

func TestReadWriteFollowRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("from-ws"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "marker.txt"), []byte("from-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(base)

	res := reg.Execute(runws.With(t.Context(), ws), "Read", map[string]any{"file_path": "marker.txt"})
	if !strings.Contains(res.Content, "from-ws") {
		t.Fatalf("Read under run workspace = %q, want from-ws", res.Content)
	}
	res = reg.Execute(runws.With(t.Context(), ws), "Write", map[string]any{"file_path": "out.txt", "content": "ws"})
	if strings.Contains(res.Content, "Error") {
		t.Fatalf("Write under run workspace failed: %q", res.Content)
	}
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err != nil {
		t.Fatalf("Write landed outside the run workspace: %v", err)
	}
}

func TestGlobFollowRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "only-in-ws.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(base)

	res := reg.Execute(runws.With(t.Context(), ws), "Glob", map[string]any{"pattern": "*.md"})
	if !strings.Contains(res.Content, "only-in-ws.md") {
		t.Fatalf("Glob under run workspace = %q, want only-in-ws.md", res.Content)
	}
}

func TestWorktreeScopedPerRunWorkspace(t *testing.T) {
	base := t.TempDir()
	wsA := t.TempDir()
	wsB := t.TempDir()
	wtA := filepath.Join(wsA, ".woca-worktrees", "fix")
	wtB := filepath.Join(wsB, ".woca-worktrees", "fix")
	d := NewDir(base)

	ctxA := runws.With(t.Context(), wsA)
	ctxB := runws.With(t.Context(), wsB)

	// Each run's worktree switch is isolated to that run's workspace.
	d.Enter(ctxA, wtA)
	if got := d.For(ctxA); got != wtA {
		t.Fatalf("For(ctxA) = %q, want %q", got, wtA)
	}
	if got := d.For(ctxB); got != wsB {
		t.Fatalf("For(ctxB) = %q, want %q (unaffected by A's worktree)", got, wsB)
	}
	d.Enter(ctxB, wtB)
	if got := d.For(ctxB); got != wtB {
		t.Fatalf("For(ctxB) = %q, want %q", got, wtB)
	}

	// Exit restores only the exiting run's workspace.
	cur, ok := d.Exit(ctxA)
	if !ok || cur != wtA {
		t.Fatalf("Exit(ctxA) = %q, %v; want %q, true", cur, ok, wtA)
	}
	if got := d.For(ctxA); got != wsA {
		t.Fatalf("For(ctxA) after exit = %q, want %q", got, wsA)
	}
	if got := d.For(ctxB); got != wtB {
		t.Fatalf("For(ctxB) after A's exit = %q, want %q", got, wtB)
	}

	// TUI compatibility: without a run workspace the base session behaves as
	// the single worktree slot it always was.
	plain := t.Context()
	d.Enter(plain, filepath.Join(base, "wt-main"))
	if got := d.For(plain); got != filepath.Join(base, "wt-main") {
		t.Fatalf("For(plain) = %q, want the base worktree", got)
	}
	if got := d.For(ctxA); got != wsA {
		t.Fatalf("For(ctxA) = %q, want %q (base worktree must not leak)", got, wsA)
	}
	if _, ok := d.Exit(plain); !ok {
		t.Fatal("Exit(plain) should report the base worktree")
	}
	if got := d.For(plain); got != base {
		t.Fatalf("For(plain) after exit = %q, want %q", got, base)
	}
}

func TestDirFallbacks(t *testing.T) {
	base := t.TempDir()
	d := NewDir(base)
	if got := d.For(t.Context()); got != base {
		t.Fatalf("For without run workspace = %q, want %q", got, base)
	}
	if got := d.BaseFor(t.Context()); got != base {
		t.Fatalf("BaseFor without run workspace = %q, want %q", got, base)
	}
	ws := t.TempDir()
	ctx := runws.With(t.Context(), ws)
	if got := d.For(ctx); got != ws {
		t.Fatalf("For with run workspace = %q, want %q", got, ws)
	}
	if got := d.BaseFor(ctx); got != ws {
		t.Fatalf("BaseFor with run workspace = %q, want %q", got, ws)
	}
}
