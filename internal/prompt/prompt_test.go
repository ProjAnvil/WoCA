package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildLoadsMemory(t *testing.T) {
	isolatedHome(t)
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
	isolatedHome(t)
	got := Build(t.TempDir(), "", "")
	if strings.Contains(got, "Project instructions") {
		t.Fatalf("should not include project instructions when no memory file exists:\n%s", got)
	}
}

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
