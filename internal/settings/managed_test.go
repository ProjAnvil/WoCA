package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedOverridesAndLocks(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// User preference first.
	if err := st.Save(Settings{Model: "user-model"}); err != nil {
		t.Fatal(err)
	}
	st.SetManaged(Settings{Provider: "openai", Model: "managed-model"})

	// Reads: managed wins.
	got := st.Load()
	if got.Model != "managed-model" || got.Provider != "openai" {
		t.Fatalf("load = %+v", got)
	}

	// Writes to locked fields are rejected…
	if err := st.Save(Settings{Model: "user-model-2"}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("model write should be locked, got %v", err)
	}
	if err := st.Save(Settings{Provider: "anthropic"}); err == nil {
		t.Fatal("provider write should be locked")
	}
	// …but untouched fields stay writable.
	if err := st.Save(Settings{CheckpointDir: "/tmp/cks"}); err != nil {
		t.Fatalf("unmanaged field write: %v", err)
	}

	// Clearing the overlay restores write access.
	st.SetManaged(Settings{})
	if err := st.Save(Settings{Model: "user-model-3"}); err != nil {
		t.Fatalf("write after clearing overlay: %v", err)
	}
}

func TestLoadManagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.json")
	os.WriteFile(path, []byte(`{"provider":"openai","max_tokens":4096}`), 0o644)
	m, err := LoadManaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "openai" || m.MaxTokens != 4096 {
		t.Fatalf("managed = %+v", m)
	}

	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := LoadManaged(path); err == nil {
		t.Fatal("invalid JSON should fail")
	}
	if _, err := LoadManaged(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file should fail")
	}
}
