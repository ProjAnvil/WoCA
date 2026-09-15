package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newWorktreeAgent builds an agent rooted in a fresh git repository.
func newWorktreeAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")

	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWorktreeEnterSwitchAndExit(t *testing.T) {
	a := newWorktreeAgent(t)
	enter, ok := a.toolByName["EnterWorktree"]
	if !ok {
		t.Fatal("EnterWorktree not registered")
	}
	exit := a.toolByName["ExitWorktree"]
	write := a.toolByName["Write"]
	base := a.registry.Dir().Base()

	res, err := enter.Invoke(t.Context(), map[string]any{"name": "feature-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Entered worktree feature-x") {
		t.Fatalf("enter = %q", res.Content)
	}

	// Every tool now resolves against the worktree: a Write of a relative
	// path lands inside it.
	if _, err := write.Invoke(t.Context(), map[string]any{
		"file_path": "note.txt", "content": "in worktree",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, ".woca-worktrees", "feature-x", "note.txt")); err != nil {
		t.Fatalf("write did not land in the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "note.txt")); err == nil {
		t.Fatal("write leaked into the main checkout")
	}

	// Double-enter is rejected.
	res, _ = enter.Invoke(t.Context(), map[string]any{"name": "another"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("double enter = %q", res.Content)
	}

	// Exit returns tools to the base directory.
	if _, err := exit.Invoke(t.Context(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := a.registry.Dir().For(t.Context()); got != base {
		t.Fatalf("current = %s, want base %s", got, base)
	}
	_, err = write.Invoke(t.Context(), map[string]any{"file_path": "base-note.txt", "content": "back"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "base-note.txt")); err != nil {
		t.Fatalf("write after exit should land in base: %v", err)
	}
}

func TestWorktreeExitRemoves(t *testing.T) {
	a := newWorktreeAgent(t)
	enter := a.toolByName["EnterWorktree"]
	exit := a.toolByName["ExitWorktree"]
	base := a.registry.Dir().Base()

	if _, err := enter.Invoke(t.Context(), map[string]any{"name": "tmp"}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(base, ".woca-worktrees", "tmp")
	if _, err := os.Stat(wt); err != nil {
		t.Fatal(err)
	}
	res, err := exit.Invoke(t.Context(), map[string]any{"remove": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "removed") {
		t.Fatalf("exit = %q", res.Content)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed: %v", err)
	}
}

func TestWorktreeRequiresGitRepo(t *testing.T) {
	plain := t.TempDir()
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(plain),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        plain,
	})
	if err != nil {
		t.Fatal(err)
	}
	enter := a.toolByName["EnterWorktree"]
	res, err := enter.Invoke(t.Context(), map[string]any{"name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "not a git repository") {
		t.Fatalf("expected git error: %q", res.Content)
	}
}
