package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildLoadsMemory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Rules\nAlways test."), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Build(dir, "extra instructions here", "test-model")
	if !strings.Contains(got, "Always test.") {
		t.Fatalf("missing CLAUDE.md content:\n%s", got)
	}
	if !strings.Contains(got, "extra instructions here") {
		t.Fatalf("missing extra prompt:\n%s", got)
	}
	if !strings.Contains(got, "woca") {
		t.Fatalf("missing base prompt")
	}
}

func TestBuildSkipsMissingMemory(t *testing.T) {
	got := Build(t.TempDir(), "", "")
	if strings.Contains(got, "Project instructions") {
		t.Fatalf("should not include project instructions when no memory file exists:\n%s", got)
	}
}
