//go:build complab

package complab

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// TestToolRegistryExposesCoreTools checks the tool set the agent gets is whole.
//
// It looks like asserting constants, but it guards a real failure mode: tool
// registration happens in defaultTools, so a panicking constructor or a renamed
// tool shows up here.
func TestToolRegistryExposesCoreTools(t *testing.T) {
	reg := newRegistry(t)
	for _, name := range []string{
		"Bash", "Read", "Write", "Edit", "Glob", "Grep", "ListDir",
		"WebFetch", "WebSearch", "TodoWrite",
	} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry does not expose tool %q", name)
		}
	}
}

// TestFileToolsRoundTrip checks the file tools really touch the disk instead of
// returning tidy empty responses.
func TestFileToolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	reg := tools.NewRegistry(dir)
	path := filepath.Join(dir, "complab.txt")

	write := toolNamed(t, reg, "Write")
	if _, err := invoke(t, write, map[string]any{
		"file_path": path,
		"content":   "woca complab probe\nsecond line\n",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	read := toolNamed(t, reg, "Read")
	got, err := invoke(t, read, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "woca complab probe") {
		t.Fatalf("Read did not return what was written: %q", truncate(got, 300))
	}

	edit := toolNamed(t, reg, "Edit")
	if _, err := invoke(t, edit, map[string]any{
		"file_path":  path,
		"old_string": "probe",
		"new_string": "edited",
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got, err = invoke(t, read, map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("Read after Edit: %v", err)
	}
	if !strings.Contains(got, "complab edited") || strings.Contains(got, "complab probe") {
		t.Fatalf("Edit did not take effect: %q", truncate(got, 300))
	}

	// Grep / Glob / ListDir see the same workspace, so chaining them shows the
	// path binding is right.
	grep := toolNamed(t, reg, "Grep")
	got, err = invoke(t, grep, map[string]any{"pattern": "complab edited", "path": dir})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if !strings.Contains(got, "complab edited") {
		t.Fatalf("Grep did not match: %q", truncate(got, 300))
	}

	glob := toolNamed(t, reg, "Glob")
	got, err = invoke(t, glob, map[string]any{"pattern": "*.txt", "path": dir})
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if !strings.Contains(got, "complab.txt") {
		t.Fatalf("Glob did not list the file that was written: %q", truncate(got, 300))
	}

	listDir := toolNamed(t, reg, "ListDir")
	if _, err := invoke(t, listDir, map[string]any{"path": dir}); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
}

// TestBashToolRunsCommand checks the shell tool really executes through the
// default executor.
func TestBashToolRunsCommand(t *testing.T) {
	reg := newRegistry(t)
	bash := toolNamed(t, reg, "Bash")
	got, err := invoke(t, bash, map[string]any{"command": "echo complab-ok"})
	if err != nil {
		t.Fatalf("Bash: %v", err)
	}
	if !strings.Contains(got, "complab-ok") {
		t.Fatalf("Bash output missing the expected text: %q", truncate(got, 300))
	}
}

// TestSettingsStoreRoundTrip checks the settings store opens, writes and
// persists.
//
// WOCA_HOME is redirected to a temp dir: settings.Open generates the master key
// there, and without the redirect it would write into the user's home.
func TestSettingsStoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WOCA_HOME", home)

	dbPath := filepath.Join(home, "woca.db")
	st, err := settings.Open(dbPath)
	if err != nil {
		t.Fatalf("settings.Open: %v", err)
	}
	if st == nil {
		t.Fatal("settings.Open returned a nil store")
	}
	defer func() { _ = st.Close() }()

	// Plaintext settings write/read.
	cfg := st.Load()
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if st.Path() == "" {
		t.Error("Store.Path() is empty")
	}

	// Secrets are stored encrypted, so a round trip proves the crypto layer
	// actually works.
	const secretName, secretValue = "complab.probe", "s3cret-value"
	if err := st.SetSecret(secretName, secretValue); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if got := st.GetSecret(secretName); got != secretValue {
		t.Fatalf("GetSecret = %q, want %q", got, secretValue)
	}
	if !st.HasSecret(secretName) {
		t.Error("HasSecret should be true")
	}
	_ = st.Close()

	// Reopen: proves it hit the disk rather than staying in memory.
	reopened, err := settings.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen settings.Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.GetSecret(secretName); got != secretValue {
		t.Fatalf("GetSecret after reopen = %q, want %q (not persisted)", got, secretValue)
	}
}
