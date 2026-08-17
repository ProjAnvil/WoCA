package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/merge"
)

func writeMem(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)
	return home
}

func pathsOf(files []memoryFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func TestCollectMemoryFilesOrder(t *testing.T) {
	home := isolatedHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(root, "pkg")
	if err := os.MkdirAll(mid, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMem(t, filepath.Join(home, ".woca", "CLAUDE.md"), "user rules")
	writeMem(t, filepath.Join(root, "CLAUDE.md"), "root rules")
	writeMem(t, filepath.Join(root, "AGENTS.md"), "root agents")
	writeMem(t, filepath.Join(mid, "CLAUDE.md"), "mid rules")
	writeMem(t, filepath.Join(mid, ".claude", "CLAUDE.md"), "mid claude")

	got := pathsOf(collectMemoryFiles(mid))
	want := []string{
		filepath.Join(home, ".woca", "CLAUDE.md"),
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(mid, "CLAUDE.md"),
		filepath.Join(mid, ".claude", "CLAUDE.md"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch:\ngot  %v\nwant %v", got, want)
		}
	}
}

func TestCollectMemoryFilesUserFallback(t *testing.T) {
	home := isolatedHome(t)
	writeMem(t, filepath.Join(home, ".claude", "CLAUDE.md"), "claude user rules")
	got := pathsOf(collectMemoryFiles(t.TempDir()))
	if len(got) != 1 || got[0] != filepath.Join(home, ".claude", "CLAUDE.md") {
		t.Fatalf("fallback to ~/.claude/CLAUDE.md failed: %v", got)
	}
}

func TestCollectMemoryFilesNoGit(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	writeMem(t, filepath.Join(dir, "CLAUDE.md"), "local rules")
	writeMem(t, filepath.Join(filepath.Dir(dir), "CLAUDE.md"), "parent must be ignored")
	got := pathsOf(collectMemoryFiles(dir))
	if len(got) != 1 || got[0] != filepath.Join(dir, "CLAUDE.md") {
		t.Fatalf("non-git dir must only read itself: %v", got)
	}
}

func TestBuildInjectsAllLayersInOrder(t *testing.T) {
	home := isolatedHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMem(t, filepath.Join(home, ".woca", "CLAUDE.md"), "ZZ_USER")
	writeMem(t, filepath.Join(root, "CLAUDE.md"), "ZZ_ROOT")
	writeMem(t, filepath.Join(root, "AGENTS.md"), "ZZ_ROOT_AGENTS")
	got := Build(filepath.Join(root), "", "")
	iu := strings.Index(got, "ZZ_USER")
	ir := strings.Index(got, "ZZ_ROOT")
	ira := strings.Index(got, "ZZ_ROOT_AGENTS")
	if iu < 0 || ir < 0 || ira < 0 {
		t.Fatalf("missing layers in prompt:\n%s", got)
	}
	if !(iu < ir && ir < ira) {
		t.Fatalf("user must precede root; CLAUDE.md precede AGENTS.md (iu=%d ir=%d ira=%d)", iu, ir, ira)
	}
	if !strings.Contains(got, "# User instructions (CLAUDE.md)") {
		t.Fatalf("user header missing:\n%s", got)
	}
}
