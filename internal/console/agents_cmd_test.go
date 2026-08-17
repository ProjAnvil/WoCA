package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/agents"
)

func TestAgentsAndPluginsCommands(t *testing.T) {
	c, _ := newFullConsole(t)
	home := t.TempDir()
	ctx := context.Background()
	s := &Session{}

	res, err := c.Execute(ctx, "agents", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a definition the agent carries.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "carried", Description: "carried agent", Prompt: "p"}})
	res, err = c.Execute(ctx, "agents", nil, s)
	if err != nil || !strings.Contains(res.Output, "carried") {
		t.Fatalf("agents with defs = %+v %v", res, err)
	}
	// Create a definition file.
	if err := os.MkdirAll(c.deps.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: helper\ndescription: helps\n---\nYou help."
	if err := os.WriteFile(filepath.Join(c.deps.AgentsDir, "helper.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = c.Execute(ctx, "agents", nil, s)
	if err != nil || !strings.Contains(res.Output, "subagent definitions") {
		t.Fatalf("agents = %+v %v", res, err)
	}
	// /agents new writes a definition.
	res, err = c.Execute(ctx, "agents", []string{"new", "writer"}, s)
	if err != nil {
		t.Fatalf("agents new = %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(c.deps.AgentsDir, "writer.md")); err != nil {
		t.Fatalf("writer.md: %v", err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"new"}, s); err == nil {
		t.Fatal("expected usage error")
	}

	// /plugins lists marketplace plugins from an isolated root.
	c.deps.PluginsRoot = filepath.Join(home, "plugins")
	res, err = c.Execute(ctx, "plugins", nil, s)
	if err != nil || !strings.Contains(res.Output, "no plugins installed") {
		t.Fatalf("plugins = %+v %v", res, err)
	}
	pluginSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginSrc, "plugin.json"), []byte(`{"name":"demo","version":"1.0.0","description":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install", pluginSrc}, s); err != nil {
		t.Fatalf("plugins install = %v", err)
	}
	res, err = c.Execute(ctx, "plugins", nil, s)
	if err != nil || !strings.Contains(res.Output, "demo") {
		t.Fatalf("plugins after install = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install"}, s); err == nil {
		t.Fatal("expected install usage error")
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install", "/missing/plugin"}, s); err == nil {
		t.Fatal("expected install error")
	}
	// Removing the installed plugin works; removing a missing one errors.
	res, err = c.Execute(ctx, "plugins", []string{"rm", "demo"}, s)
	if err != nil {
		t.Fatalf("plugins rm = %+v %v", res, err)
	}
	// Removing a plugin that is not installed is a tolerated no-op.
	if _, err := c.Execute(ctx, "plugins", []string{"rm", "not-there"}, s); err != nil {
		t.Fatalf("rm missing = %v", err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"bogus"}, s); err == nil {
		t.Fatal("expected usage error")
	}
	// Without settings, listing from the (empty) default root still works.
	c2, _ := newAgentConsole(t, nil)
	c2.deps.PluginsRoot = filepath.Join(t.TempDir(), "plugins")
	if _, err := c2.Execute(ctx, "plugins", nil, s); err != nil {
		t.Fatalf("plugins without settings = %v", err)
	}
}

func TestAgentsCommandDetails(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	// /agents <name> renders the definition; unknown names error.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{
		Name: "codeagent", Description: "writes code", Prompt: "You write code.",
		Source: "/defs/codeagent.md",
	}})
	res, err := c.Execute(ctx, "agents", []string{"show", "codeagent"}, s)
	if err != nil || !strings.Contains(res.Output, "You write code.") {
		t.Fatalf("agents show = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"show", "ghost"}, s); err == nil {
		t.Fatal("expected unknown agent error")
	}
	// new: invalid name, duplicate, description variant.
	if _, err := c.Execute(ctx, "agents", []string{"new"}, s); err == nil {
		t.Fatal("expected usage error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "..."}, s); err == nil {
		t.Fatal("expected invalid name error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "writer", "writes", "docs"}, s); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "writer"}, s); err == nil {
		t.Fatal("expected duplicate error")
	}
	// rm: missing name, unknown agent, source-less definition.
	if _, err := c.Execute(ctx, "agents", []string{"rm"}, s); err == nil {
		t.Fatal("expected rm usage error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"rm", "ghost"}, s); err == nil {
		t.Fatal("expected unknown rm error")
	}
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "inline"}})
	if _, err := c.Execute(ctx, "agents", []string{"rm", "inline"}, s); err == nil {
		t.Fatal("expected no-source error")
	}
	// rm removes a file-backed definition.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "filebacked", Source: filepath.Join(c.deps.AgentsDir, "writer.md")}})
	if _, err := c.Execute(ctx, "agents", []string{"rm", "filebacked"}, s); err != nil {
		t.Fatalf("rm filebacked = %v", err)
	}
}
