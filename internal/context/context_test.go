package contextx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	if got := Collect(dir); got != "" {
		t.Fatalf("expected empty context for non-git dir, got %q", got)
	}
}

func TestCollectGitRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.Command("git", "init", "-q", dir).Output(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Collect(dir)
	if !strings.Contains(got, "<git_status>") {
		t.Fatalf("expected git_status block, got %q", got)
	}
	if !strings.Contains(got, "a.txt") {
		t.Fatalf("expected a.txt in status, got %q", got)
	}
}

func TestCollectGitStatusSections(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// A dirty working tree produces a non-empty status section.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Collect(dir)
	if !strings.Contains(got, "<git_status>") || !strings.Contains(got, "dirty.txt") {
		t.Fatalf("Collect = %q", got)
	}
	// Not a repository: empty.
	if got := Collect(t.TempDir()); got != "" {
		t.Fatalf("non-repo Collect = %q", got)
	}
}
