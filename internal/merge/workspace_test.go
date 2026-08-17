package merge

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkspaceDirsStopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	ResetWorkspaceCache()
	got := WorkspaceDirs(deep)
	want := []string{deep, filepath.Join(root, "a"), root}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWorkspaceDirsWorktreeGitFile(t *testing.T) {
	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ResetWorkspaceCache()
	got := WorkspaceDirs(sub)
	want := []string{sub, wt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worktree .git file must be recognized, got %v, want %v", got, want)
	}
}

func TestWorkspaceDirsNonGitOnlyWorkDir(t *testing.T) {
	dir := t.TempDir()
	ResetWorkspaceCache()
	got := WorkspaceDirs(dir)
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("non-git dir must yield itself only, got %v", got)
	}
}

func TestWorkspaceDirsReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	ResetWorkspaceCache()
	WorkspaceDirs(dir)
	got := WorkspaceDirs(dir)
	got[0] = "/mutated"
	if again := WorkspaceDirs(dir); again[0] != dir {
		t.Fatalf("cache was mutated: %v", again)
	}
}
