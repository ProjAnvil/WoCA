package contextx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
