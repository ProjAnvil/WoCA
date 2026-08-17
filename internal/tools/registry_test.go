package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCategoryFlags(t *testing.T) {
	cases := []struct {
		c             Category
		readOnly, saf bool
	}{
		{CatRead, true, true},
		{CatWrite, false, false},
		{CatShell, false, false},
		{CatWeb, true, true},
		{CatMeta, false, true},
	}
	for _, c := range cases {
		if got := c.c.ReadOnly(); got != c.readOnly {
			t.Errorf("Category(%d).ReadOnly() = %v, want %v", c.c, got, c.readOnly)
		}
		if got := c.c.Safe(); got != c.saf {
			t.Errorf("Category(%d).Safe() = %v, want %v", c.c, got, c.saf)
		}
	}
}

func TestCategoryOfUnknownDefaultsToShell(t *testing.T) {
	if got := CategoryOf("DefinitelyNotATool"); got != CatShell {
		t.Fatalf("CategoryOf(unknown) = %v, want CatShell", got)
	}
	if got := CategoryOf("Read"); got != CatRead {
		t.Fatalf("CategoryOf(Read) = %v, want CatRead", got)
	}
}

func TestIsReadOnlyInputSensitive(t *testing.T) {
	if IsReadOnly("Write", nil) {
		t.Error("Write must not be read-only")
	}
	if IsReadOnly("EnterWorktree", nil) {
		t.Error("unknown-to-map shell tools must not be read-only")
	}
	// Bash without a string command falls back to non-read-only.
	if IsReadOnly("Bash", nil) {
		t.Error("Bash without args must not be read-only")
	}
	if IsReadOnly("Bash", map[string]any{"command": 42}) {
		t.Error("Bash with non-string command must not be read-only")
	}
	if !IsReadOnly("Grep", nil) {
		t.Error("Grep must be read-only")
	}
}

func TestRegistryDefaultWorkdirAndAccessors(t *testing.T) {
	// Empty workdir falls back to "." (resolved against the test cwd).
	reg := NewRegistry("")
	if got := reg.Dir().Base(); got != "." {
		t.Fatalf("Base() = %q, want %q", got, ".")
	}
	if reg.Dir() == nil {
		t.Fatal("Dir() = nil")
	}

	dir := t.TempDir()
	reg = NewRegistry(dir)
	names := make([]string, 0)
	for _, tl := range reg.All() {
		names = append(names, tl.Name())
	}
	if len(names) == 0 {
		t.Fatal("All() returned no tools")
	}
	// All preserves registration order: the first default tool is Bash.
	if names[0] != "Bash" {
		t.Fatalf("All()[0] = %q, want Bash", names[0])
	}
	for _, want := range []string{"Read", "Write", "Edit", "NotebookEdit", "Glob", "Grep", "TodoWrite", "WebFetch", "WebSearch", "LSP"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default registry missing %q (has %v)", want, names)
		}
	}
}

func TestRegistryRegisterAndExecute(t *testing.T) {
	reg := NewRegistry(t.TempDir())

	reg.Register(nil) // no-op
	reg.Register(AskUserQuestionTool())
	if _, ok := reg.Get("AskUserQuestion"); !ok {
		t.Fatal("AskUserQuestion not registered")
	}
	// Registering again replaces without duplicating the order entry.
	reg.Register(AskUserQuestionTool())
	count := 0
	for _, tl := range reg.All() {
		if tl.Name() == "AskUserQuestion" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("AskUserQuestion appears %d times in All()", count)
	}

	// Unknown tools and tool-level errors become result content, not errors.
	res := reg.Execute(context.Background(), "NoSuchTool", nil)
	if !strings.Contains(res.Content, `Error: unknown tool "NoSuchTool"`) {
		t.Fatalf("unknown tool = %q", res.Content)
	}
	res = reg.Execute(context.Background(), "Read", map[string]any{"file_path": "definitely-missing.txt"})
	if !strings.Contains(res.Content, "Error:") || !strings.Contains(res.Content, "definitely-missing.txt") {
		t.Fatalf("tool error content = %q", res.Content)
	}
	if err := os.WriteFile(filepath.Join(reg.Dir().Base(), "ok.txt"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = reg.Execute(context.Background(), "Read", map[string]any{"file_path": "ok.txt"})
	if strings.Contains(res.Content, "Error") {
		t.Fatalf("successful execute = %q", res.Content)
	}
}

func TestSortedEntries(t *testing.T) {
	if got := sortedEntries([]string{"c", "a", "b"}); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("sortedEntries = %v", got)
	}
}

func TestDirEmptyWorkdirAndEnter(t *testing.T) {
	d := NewDir("")
	if got := d.Base(); got != "." {
		t.Fatalf("NewDir(\"\").Base() = %q, want .", got)
	}
	// Entering an empty worktree is a no-op.
	d.Enter(context.Background(), "")
	if got := d.For(context.Background()); got != "." {
		t.Fatalf("For after empty Enter = %q", got)
	}
	if _, ok := d.Exit(context.Background()); ok {
		t.Fatal("Exit without Enter reported a worktree")
	}
}

func TestGrepUnreadableEntriesSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not block reads")
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("matchme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	// An unreadable subdirectory surfaces as a walk error for one entry;
	// the walk keeps going instead of failing the search.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "inner.txt"), []byte("matchme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(blocked, 0o755)
		os.Chmod(secret, 0o644)
	})
	// Unreadable files and dirs are skipped, not fatal.
	reg := NewRegistry(dir)
	res := reg.Execute(context.Background(), "Grep", map[string]any{"pattern": "matchme", "path": "."})
	if strings.Contains(res.Content, "matchme") {
		t.Fatalf("unreadable entries were searched: %q", res.Content)
	}
	if strings.Contains(res.Content, "Error") {
		t.Fatalf("unreadable entries must not fail the walk: %q", res.Content)
	}
}
