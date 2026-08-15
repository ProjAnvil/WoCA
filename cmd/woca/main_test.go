package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name                   string
		mode                   string
		skip, accept, ro, plan bool
		want                   config.PermissionMode
	}{
		{"default", "", false, false, false, false, config.ModeDefault},
		{"bypass", "", true, false, false, false, config.ModeBypass},
		{"accept", "", false, true, false, false, config.ModeAcceptEdits},
		{"readonly", "", false, false, true, false, config.ModeReadOnly},
		{"plan", "", false, false, false, true, config.ModePlan},
		{"explicit", "bypassPermissions", false, false, false, false, config.ModeBypass},
	}
	for _, c := range cases {
		if got := resolveMode(c.mode, c.skip, c.accept, c.ro, c.plan); got != c.want {
			t.Errorf("%s: resolveMode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLoadPermissions(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".woca", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"permissions":{"deny":["Bash(rm *)"],"allow":["Bash(git *)","Read"]}}`
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := loadPermissions(dir)
	if rules == nil {
		t.Fatal("rules should not be nil")
	}
	if action, ok := rules.Action("Bash", map[string]any{"command": "rm -rf /"}); !ok || action != "deny" {
		t.Fatalf("rm should be denied: %q %v", action, ok)
	}
	if action, ok := rules.Action("Bash", map[string]any{"command": "git status"}); !ok || action != "allow" {
		t.Fatalf("git should be allowed: %q %v", action, ok)
	}
}

func TestTodosTasksConversionRoundTrip(t *testing.T) {
	todos := []tools.Todo{{Content: "a", Status: "in_progress", ActiveForm: "doing a"}}
	tasks := []agent.TaskRecord{{ID: "task-3", Subject: "s", Description: "d", Status: agent.TaskRecordCompleted}}

	if got := todosFromSession(todosToSession(todos)); len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("todos round-trip = %+v", got)
	}
	if got := tasksFromSession(tasksToSession(tasks)); len(got) != 1 || got[0].ID != "task-3" || got[0].Status != agent.TaskRecordCompleted {
		t.Fatalf("tasks round-trip = %+v", got)
	}
}

func TestLoadHooksConfig(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".woca", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadHooksConfig(dir)
	if len(cfg.PreToolUse) != 1 || cfg.PreToolUse[0].Hooks[0].Command != "echo hi" {
		t.Fatalf("hooks = %+v", cfg)
	}
}

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".woca", "skills", "commit")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: commit\ndescription: commit\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := loadSkills(dir)
	if len(list) != 1 || list[0].Name != "commit" {
		t.Fatalf("skills = %+v", list)
	}
}
