package claudeenv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// setHomeError points homeDir at a permanent failure.
func setHomeError(t *testing.T) {
	t.Helper()
	orig := homeDir
	homeDir = func() (string, error) { return "", errors.New("homeless") }
	t.Cleanup(func() { homeDir = orig })
}

// Every home-dependent entry point must propagate the home lookup error.
func TestHomeLookupErrorsPropagate(t *testing.T) {
	setHomeError(t)
	if _, err := SkillsDir(); err == nil {
		t.Error("SkillsDir accepted a failing home lookup")
	}
	if _, err := Skills(); err == nil {
		t.Error("Skills accepted a failing home lookup")
	}
	if _, err := SyncSkills(t.TempDir(), nil); err == nil {
		t.Error("SyncSkills accepted a failing home lookup")
	}
	if _, err := claudeJSONPath(); err == nil {
		t.Error("claudeJSONPath accepted a failing home lookup")
	}
	if _, err := Servers(); err == nil {
		t.Error("Servers accepted a failing home lookup")
	}
}

// ~/.claude/skills existing as a regular file is a read error, not "missing".
func TestSkillsReadDirError(t *testing.T) {
	home := setHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Skills(); err == nil {
		t.Fatal("Skills accepted a skills path that is a file")
	}
}

// ~/.claude.json existing as a directory is a read error, not "missing".
func TestServersReadFileError(t *testing.T) {
	home := setHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Servers(); err == nil {
		t.Fatal("Servers accepted a .claude.json that is a directory")
	}
}

// skipIfRoot: permission-denied fixtures do not fail for root.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denied fixtures are ineffective")
	}
}

// Listing the dest dir requires read access to it.
func TestSyncSkillsReadDirError(t *testing.T) {
	skipIfRoot(t)
	home := setHome(t)
	writeSkill(t, home, "alpha", "")

	woca := filepath.Join(home, ".woca")
	destDir := filepath.Join(woca, "skills")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(destDir, 0o755) })

	if _, err := SyncSkills(woca, []string{"alpha"}); err == nil {
		t.Fatal("SyncSkills accepted an unreadable dest dir")
	}
}

// Removing a deselected managed link requires write access to the dest dir.
func TestSyncSkillsRemoveError(t *testing.T) {
	skipIfRoot(t)
	home := setHome(t)
	writeSkill(t, home, "alpha", "")

	woca := filepath.Join(home, ".woca")
	if _, err := SyncSkills(woca, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(woca, "skills")
	if err := os.Chmod(destDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(destDir, 0o755) })

	if _, err := SyncSkills(woca, nil); err == nil {
		t.Fatal("SyncSkills accepted an unremovable stale link")
	}
}

// Creating the symlink requires write access to the dest dir.
func TestSyncSkillsSymlinkError(t *testing.T) {
	skipIfRoot(t)
	home := setHome(t)
	writeSkill(t, home, "alpha", "")

	woca := filepath.Join(home, ".woca")
	destDir := filepath.Join(woca, "skills")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(destDir, 0o755) })

	linked, err := SyncSkills(woca, []string{"alpha"})
	if err == nil {
		t.Fatal("SyncSkills accepted an unwritable dest dir")
	}
	if len(linked) != 0 {
		t.Fatalf("linked = %v, want none", linked)
	}
}

// Writing mcp.json.tmp requires write access to the woca root.
func TestWriteMCPConfigWriteError(t *testing.T) {
	skipIfRoot(t)
	woca := t.TempDir()
	if err := os.Chmod(woca, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(woca, 0o755) })

	srv := MCPServer{Name: "x", Type: "http", URL: "https://x/mcp"}
	if err := WriteMCPConfig(woca, []MCPServer{srv}); err == nil {
		t.Fatal("WriteMCPConfig accepted an unwritable root")
	}
}

// mcp.json existing as a directory is a read error, not "missing".
func TestLoadMCPConfigReadError(t *testing.T) {
	woca := t.TempDir()
	if err := os.MkdirAll(filepath.Join(woca, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCPConfig(woca); err == nil {
		t.Fatal("LoadMCPConfig accepted an mcp.json that is a directory")
	}
}

// Entries that no longer parse are skipped, not fatal.
func TestLoadMCPConfigSkipsUnparsableEntry(t *testing.T) {
	woca := t.TempDir()
	doc := `{"mcpServers":{
		"bad":  {"type":"carrier-pigeon"},
		"good": {"type":"http","url":"https://x/mcp"}
	}}`
	if err := os.WriteFile(filepath.Join(woca, "mcp.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := LoadMCPConfig(woca)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0] != "good=https://x/mcp" {
		t.Fatalf("specs = %v, want [good=https://x/mcp]", specs)
	}
}
