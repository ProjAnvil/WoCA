package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
)

func loadTestAgents(dir string) ([]agents.Definition, error) { return agents.Load(dir) }

func TestOutputStyleCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	st, err := settings.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c.deps.Settings = st

	res, err := c.Execute(context.Background(), "output-style", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"default", "explanatory", "learning"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("style list missing %q:\n%s", want, res.Output)
		}
	}

	if _, err := c.Execute(context.Background(), "output-style", []string{"explanatory"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.deps.Agent.SystemPrompt(), "Output style: Explanatory") {
		t.Fatal("live system prompt not restyled")
	}
	if got := st.Load().OutputStyle; got != "explanatory" {
		t.Fatalf("persisted style = %q", got)
	}

	// Back to default clears the persisted key and the prompt section.
	if _, err := c.Execute(context.Background(), "output-style", []string{"default"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.deps.Agent.SystemPrompt(), "Output style:") {
		t.Fatal("default style should not carry a style section")
	}
	if got := st.Load().OutputStyle; got != "" {
		t.Fatalf("style not cleared: %q", got)
	}

	if _, err := c.Execute(context.Background(), "output-style", []string{"bogus"}, nil); err == nil {
		t.Fatal("unknown style should fail")
	}
}

func TestPluginsCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	root := filepath.Join(t.TempDir(), "plugins")
	c.deps.PluginsRoot = root

	// Empty listing.
	res, err := c.Execute(context.Background(), "plugins", nil, nil)
	if err != nil || !strings.Contains(res.Output, "no plugins installed") {
		t.Fatalf("empty = %q err=%v", res.Output, err)
	}

	// Build a source plugin and install it via the command.
	src := t.TempDir()
	pdir := filepath.Join(src, "acme")
	if err := os.MkdirAll(filepath.Join(pdir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pdir, "plugin.json"), []byte(`{"name":"acme","version":"1.0.0","description":"test"}`), 0o644)
	os.WriteFile(filepath.Join(pdir, "agents", "helper.md"), []byte("---\nname: helper\ndescription: Helps\n---\nHelp out.\n"), 0o644)

	if _, err := c.Execute(context.Background(), "plugins", []string{"install", pdir}, nil); err != nil {
		t.Fatal(err)
	}
	// The plugin's agent is live immediately.
	defs := c.deps.Agent.AgentDefinitions()
	found := false
	for _, d := range defs {
		if d.Name == "helper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plugin agent not live: %+v", defs)
	}

	res, err = c.Execute(context.Background(), "plugins", nil, nil)
	if err != nil || !strings.Contains(res.Output, "acme") || !strings.Contains(res.Output, "1 agent") {
		t.Fatalf("list after install = %q err=%v", res.Output, err)
	}

	if _, err := c.Execute(context.Background(), "plugins", []string{"rm", "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Execute(context.Background(), "plugins", nil, nil)
	if !strings.Contains(res.Output, "no plugins installed") {
		t.Fatalf("list after rm = %q", res.Output)
	}
	if _, err := c.Execute(context.Background(), "plugins", []string{"bogus"}, nil); err == nil {
		t.Fatal("unknown subcommand should fail")
	}
}

func TestDoctorReportsChecks(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	c.deps.SessionDir = filepath.Join(t.TempDir(), "sessions")
	res, err := c.Execute(context.Background(), "doctor", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workdir", "tools", "model", "settings db", "credentials", "session storage", "memory store", "hooks", "permissions"} {
		if !strings.Contains(res.Output, want+":") {
			t.Fatalf("doctor missing %q:\n%s", want, res.Output)
		}
	}
	if !strings.Contains(res.Output, "0 failure(s)") {
		t.Fatalf("unexpected failures:\n%s", res.Output)
	}
}

func TestDoctorWarnsWithoutDBAndMemory(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	c.deps.SessionDir = filepath.Join(t.TempDir(), "sessions")
	c.deps.Settings = nil
	c.deps.Memory = nil
	res, err := c.Execute(context.Background(), "doctor", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "disabled") || !strings.Contains(res.Output, "not configured") {
		t.Fatalf("expected warnings:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "warning(s)") {
		t.Fatal("summary line missing")
	}
}

func TestExportMarkdown(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{
		messages.Human("hello there"),
		messages.AI("hi yourself"),
	}}
	dir := t.TempDir()
	res, err := c.Execute(context.Background(), "export", []string{filepath.Join(dir, "out.md")}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "out.md") {
		t.Fatalf("export output = %q", res.Output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"## User", "hello there", "## Assistant", "hi yourself"} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown export missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "## System") {
		t.Fatal("system prompt should not be exported")
	}
}

func TestExportJSONResumable(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{messages.Human("q"), messages.AI("a")}}
	path := filepath.Join(t.TempDir(), "out.json")
	if _, err := c.Execute(context.Background(), "export", []string{path}, s); err != nil {
		t.Fatal(err)
	}
	st, err := session.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Messages) != 2 || st.Messages[0].Content != "q" {
		t.Fatalf("json export = %+v", st.Messages)
	}
}

func TestExportEmptyFails(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	if _, err := c.Execute(context.Background(), "export", nil, &Session{}); err == nil {
		t.Fatal("exporting an empty conversation should fail")
	}
}

func TestRenameCreatesNamedSnapshot(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	dir := t.TempDir()
	c.deps.SessionDir = dir
	s := &Session{History: []messages.Message{messages.Human("one"), messages.AI("two")}}
	res, err := c.Execute(context.Background(), "rename", []string{"My Session!"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "My-Session") {
		t.Fatalf("rename output = %q", res.Output)
	}
	st, err := session.LoadState(filepath.Join(dir, "My-Session.json"))
	if err != nil || len(st.Messages) != 2 {
		t.Fatalf("named snapshot = %+v err=%v", st.Messages, err)
	}
	// Renaming again replaces the same file.
	s.History = append(s.History, messages.Human("three"))
	if _, err := c.Execute(context.Background(), "rename", []string{"My Session!"}, s); err != nil {
		t.Fatal(err)
	}
	st, _ = session.LoadState(filepath.Join(dir, "My-Session.json"))
	if len(st.Messages) != 3 {
		t.Fatalf("snapshot not refreshed: %d messages", len(st.Messages))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("rename must replace, not accumulate: %d files", len(entries))
	}
}

func TestAgentsManagement(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	c.deps.AgentsDir = filepath.Join(t.TempDir(), "agents")

	// Empty listing.
	res, err := c.Execute(context.Background(), "agents", nil, nil)
	if err != nil || !strings.Contains(res.Output, "no subagent definitions") {
		t.Fatalf("empty list = %q err=%v", res.Output, err)
	}

	// Create from template; the live agent has no definitions loaded yet
	// (they load at startup), so simulate a restart by re-registering deps:
	if _, err := c.Execute(context.Background(), "agents", []string{"new", "reviewer", "Reviews code"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.deps.AgentsDir, "reviewer.md")); err != nil {
		t.Fatal(err)
	}
	// Duplicate create fails.
	if _, err := c.Execute(context.Background(), "agents", []string{"new", "reviewer"}, nil); err == nil {
		t.Fatal("duplicate create should fail")
	}

	// Reload definitions like buildAgent would and verify show/rm.
	defs, err := loadTestAgents(c.deps.AgentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "reviewer" || defs[0].Source == "" {
		t.Fatalf("loaded defs = %+v", defs)
	}
	c.deps.Agent.SetAgentDefinitions(defs)

	res, err = c.Execute(context.Background(), "agents", nil, nil)
	if err != nil || !strings.Contains(res.Output, "reviewer") || !strings.Contains(res.Output, "Reviews code") {
		t.Fatalf("list = %q err=%v", res.Output, err)
	}
	res, err = c.Execute(context.Background(), "agents", []string{"show", "reviewer"}, nil)
	if err != nil || !strings.Contains(res.Output, "You are the reviewer subagent") {
		t.Fatalf("show = %q err=%v", res.Output, err)
	}
	if _, err := c.Execute(context.Background(), "agents", []string{"rm", "reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.deps.AgentsDir, "reviewer.md")); !os.IsNotExist(err) {
		t.Fatal("definition file not removed")
	}
	if _, err := c.Execute(context.Background(), "agents", []string{"show", "reviewer"}, nil); err == nil {
		t.Fatal("show after rm should fail")
	}
	if _, err := c.Execute(context.Background(), "agents", []string{"bogus"}, nil); err == nil {
		t.Fatal("unknown subcommand should fail")
	}
}
