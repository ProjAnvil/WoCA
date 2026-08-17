package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStyleNamesAndSubagentPrompt(t *testing.T) {
	names := StyleNames()
	if len(names) == 0 || names[0] == "" {
		t.Fatalf("StyleNames = %v", names)
	}
	sp := SubagentPrompt("/tmp/x")
	if !strings.Contains(sp, "Working directory: /tmp/x") || !strings.Contains(sp, "subagent") {
		t.Fatalf("SubagentPrompt = %q", sp)
	}
}

func TestDetectShell(t *testing.T) {
	for _, c := range []struct{ shell, want string }{
		{"/bin/zsh", "zsh"},
		{"/usr/local/bin/fish", "fish"},
		{"/opt/strange/shell", "/opt/strange/shell"},
		{"", "unknown"},
	} {
		t.Setenv("SHELL", c.shell)
		if got := DetectShell(); got != c.want {
			t.Errorf("DetectShell(%q) = %q, want %q", c.shell, got, c.want)
		}
	}
}

func TestMemoryFileSectionMissing(t *testing.T) {
	dir := t.TempDir()
	// A missing memory file reports false.
	if _, ok := memoryFileSection(dir, "CLAUDE.md"); ok {
		t.Fatal("missing file should not yield a section")
	}
	// A directory in place of the memory file is skipped.
	if err := os.MkdirAll(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := memoryFileSection(dir, "CLAUDE.md"); ok {
		t.Fatal("directory should not yield a section")
	}
	// An empty CLAUDE.md yields no section (blank content is skipped).
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "CLAUDE.md"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := memoryFileSection(dir2, "CLAUDE.md"); ok {
		t.Fatal("empty file should not yield a section")
	}
	// Content produces the section.
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "CLAUDE.md"), []byte("be terse"), 0o644); err != nil {
		t.Fatal(err)
	}
	section, ok := memoryFileSection(dir3, "CLAUDE.md")
	if !ok || !strings.Contains(section, "be terse") {
		t.Fatalf("section = %q ok=%v", section, ok)
	}
}
