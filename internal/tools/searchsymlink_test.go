package tools

// Search-scope symlink filtering (deepagents v0.7.38 parity): results of
// Glob, Grep and ListDir never carry content a symlink resolves to outside
// the search base; symlink names that stay inside keep working, and Read of
// an explicitly named path is untouched (the fix is scoped to search).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkScopeFixture builds a workspace with an escaping symlink (link →
// outside) and an inner one (inner → realdir, inside the workspace).
func symlinkScopeFixture(t *testing.T) (ws string, reg *Registry) {
	t.Helper()
	ws = t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "realdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "realdir", "inside.txt"), []byte("needle inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realdir", filepath.Join(ws, "inner")); err != nil {
		t.Fatal(err)
	}
	return ws, NewRegistry(ws)
}

func TestGlobExcludesSymlinksEscapingTheBase(t *testing.T) {
	ws, reg := symlinkScopeFixture(t)
	res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "**/secret.txt"})
	if strings.Contains(res.Content, "secret.txt") {
		t.Fatalf("glob leaked through the escaping symlink: %q", res.Content)
	}
	if !strings.Contains(res.Content, "No files matched") {
		t.Fatalf("expected the no-match placeholder, got %q", res.Content)
	}
	// The inside-resolving symlink keeps its subtree reachable.
	res = reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "**/inside.txt"})
	if !strings.Contains(res.Content, "inside.txt") {
		t.Fatalf("inside-resolving symlink lost from glob: %q", res.Content)
	}
	_ = ws
}

func TestGrepExcludesSymlinksEscapingTheBase(t *testing.T) {
	_, reg := symlinkScopeFixture(t)
	res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "needle outside"})
	if strings.Contains(res.Content, "secret.txt") {
		t.Fatalf("grep leaked through the escaping symlink: %q", res.Content)
	}
	if !strings.Contains(res.Content, "No matches found") {
		t.Fatalf("expected no matches, got %q", res.Content)
	}
	res = reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "needle"})
	if !strings.Contains(res.Content, "inside.txt") {
		t.Fatalf("inside-resolving symlink lost from grep: %q", res.Content)
	}
}

func TestListDirExcludesSymlinksEscapingTheBase(t *testing.T) {
	_, reg := symlinkScopeFixture(t)
	res := reg.Execute(t.Context(), "ListDir", map[string]any{})
	if strings.Contains(res.Content, "link") {
		t.Fatalf("listdir shows the escaping symlink: %q", res.Content)
	}
	if !strings.Contains(res.Content, "inner") || !strings.Contains(res.Content, "keep.txt") || !strings.Contains(res.Content, "realdir/") {
		t.Fatalf("listdir lost inside entries: %q", res.Content)
	}
}

// Read of an explicitly named path is not scoped by the search filter.
func TestReadThroughSymlinkStillWorks(t *testing.T) {
	_, reg := symlinkScopeFixture(t)
	res := reg.Execute(t.Context(), "Read", map[string]any{"file_path": "link/secret.txt"})
	if !strings.Contains(res.Content, "needle outside") {
		t.Fatalf("explicit read through the symlink = %q", res.Content)
	}
}
