package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/merge"
	"github.com/projanvil/woca/internal/skills"
)

// writeFileT creates path's parent dirs and writes content.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "dup", "SKILL.md"), "USER SKILL")
	writeFileT(t, filepath.Join(root, ".claude", "skills", "dup", "SKILL.md"), "ROOT CLAUDE")
	writeFileT(t, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md"), "DEEP WOCA")
	writeFileT(t, filepath.Join(deep, ".claude", "skills", "dup", "SKILL.md"), "DEEP CLAUDE")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "only-user", "SKILL.md"), "USER ONLY")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadSkills(deep, zerolog.New(&buf))

	byName := map[string]skills.Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if got := byName["dup"]; len(got.Body) == 0 || got.Body != "DEEP WOCA" {
		t.Fatalf("dup winner must be workDir .woca/skills (D2 breaking flip), got %q", got.Body)
	}
	if got := byName["only-user"]; got.Body != "USER ONLY" {
		t.Fatalf("user-only skill missing: %+v", list)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", logs)
	}
	if !strings.Contains(logs, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md")) {
		t.Fatalf("winner source not in logs: %q", logs)
	}
}

func TestLoadSkillsSameDirWocaBeatsClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir() // 无 .git：只看本目录
	writeFileT(t, filepath.Join(dir, ".woca", "skills", "dup", "SKILL.md"), "WOCA")
	writeFileT(t, filepath.Join(dir, ".claude", "skills", "dup", "SKILL.md"), "CLAUDE")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	list := loadSkills(dir, zerolog.Nop())
	if len(list) != 1 || list[0].Body != "WOCA" {
		t.Fatalf("same-dir .woca must beat .claude, got %+v", list)
	}
}

func TestLoadAgentsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: user\n---\nUSER AGENT")
	writeFileT(t, filepath.Join(deep, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: ws\n---\nWS AGENT")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "only-user.md"), "---\nname: only-user\ndescription: u\n---\nONLY USER")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadAgents(deep, zerolog.New(&buf))

	byName := map[string]string{}
	for _, d := range list {
		byName[d.Name] = d.Prompt
	}
	if byName["dup"] != "WS AGENT" {
		t.Fatalf("dup winner must be workspace, got %q", byName["dup"])
	}
	if byName["only-user"] != "ONLY USER" {
		t.Fatalf("user-only agent missing: %+v", list)
	}
	if !strings.Contains(buf.String(), "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", buf.String())
	}
}
