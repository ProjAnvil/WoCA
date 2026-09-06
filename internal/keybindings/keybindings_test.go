package keybindings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBindsComposerKeys(t *testing.T) {
	m := Default()
	if got := m.Action("ctrl+j"); got != ActionComposerNewline {
		t.Fatalf("ctrl+j = %q", got)
	}
	if got := m.Action("ctrl+g"); got != ActionComposerEditor {
		t.Fatalf("ctrl+g = %q", got)
	}
	if got := m.Action("ctrl+z"); got != "" {
		t.Fatalf("unbound key = %q, want empty", got)
	}
}

func TestOverrideReplacesDefaultAction(t *testing.T) {
	m := Default()
	m.Apply(map[string]string{"ctrl+p": ActionComposerNewline})
	if got := m.Action("ctrl+p"); got != ActionComposerNewline {
		t.Fatal(got)
	}
	// Normalization: case and surrounding space don't matter.
	if got := m.Action("  Ctrl+P  "); got != ActionComposerNewline {
		t.Fatalf("normalized lookup = %q", got)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Action("ctrl+j"); got != ActionComposerNewline {
		t.Fatalf("ctrl+j = %q", got)
	}
}

func TestLoadAppliesValidOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"ctrl+p":"composer:newline"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Action("ctrl+p"); got != ActionComposerNewline {
		t.Fatalf("ctrl+p = %q", got)
	}
	// Defaults survive alongside the override.
	if got := m.Action("ctrl+g"); got != ActionComposerEditor {
		t.Fatalf("ctrl+g = %q", got)
	}
}

func TestLoadRejectsUnknownActionFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"ctrl+p":"nonsense"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("err = %v", err)
	}
	if got := m.Action("ctrl+p"); got != "" {
		t.Fatalf("ctrl+p should stay unbound after fallback, got %q", got)
	}
	if got := m.Action("ctrl+j"); got != ActionComposerNewline {
		t.Fatalf("defaults should remain intact: ctrl+j=%q", got)
	}
}

func TestLoadRejectsMalformedJSONFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keybindings.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err == nil {
		t.Fatal("want error")
	}
	if got := m.Action("ctrl+g"); got != ActionComposerEditor {
		t.Fatalf("defaults should remain intact: ctrl+g=%q", got)
	}
}

func TestEnsureFileCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "keybindings.json")
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second call over an existing file is a no-op.
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
}
