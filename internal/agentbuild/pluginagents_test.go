package agentbuild

// Plugin agents must surface in the per-workspace agent set: serve-mode
// sessions resolve their subagent definitions through AgentsFor, so a
// plugin whose agents only reach the startup set would never appear in
// the @ list or the Task tool's advertised agents (the child registry
// includes them either way, so an explicit subagent_type call worked —
// the gap was discovery). A workspace's own same-name definition shadows
// the plugin's, matching the layering everywhere else.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/agent"
)

func writePluginWithAgent(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, ".woca", "plugins", name)
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"` + name + `","description":"test plugin","version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	md := `---
name: ` + name + `-probe
description: Probe agent contributed by the plugin.
tools: Read
---
You are the probe.
`
	if err := os.WriteFile(filepath.Join(dir, "agents", "probe.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginAgentsSurfaceInWorkspaceAgentSet(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)

	writePluginWithAgent(t, home, "probeplug")
	// A workspace agent with the SAME name must shadow the plugin's.
	wsAgentDir := filepath.Join(opts.Workdir, ".woca", "agents")
	if err := os.MkdirAll(wsAgentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shadow := `---
name: probeplug-probe
description: Workspace-local shadow of the plugin agent.
tools: Read
---
Workspace wins.
`
	if err := os.WriteFile(filepath.Join(wsAgentDir, "shadow.md"), []byte(shadow), 0o644); err != nil {
		t.Fatal(err)
	}

	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	// A DIFFERENT workspace (not the startup one) resolves through
	// AgentsFor — the serve-mode path.
	other := t.TempDir()
	defs := ap.Agent.AgentDefinitionsIn(agent.WithRunWorkspace(t.Context(), other))
	var pluginSeen, shadowFree bool
	for _, d := range defs {
		if d.Name == "probeplug-probe" {
			pluginSeen = true
			if d.Source != "plugin/probeplug" {
				t.Fatalf("workspace-set probe def source = %q, want plugin/probeplug", d.Source)
			}
		}
	}
	if !pluginSeen {
		t.Fatalf("plugin agent missing from the per-workspace set: %+v", defs)
	}

	// In the startup workspace the local definition shadows the plugin's.
	startupDefs := ap.Agent.AgentDefinitionsIn(agent.WithRunWorkspace(t.Context(), opts.Workdir))
	for _, d := range startupDefs {
		if d.Name == "probeplug-probe" && d.Source != "plugin/probeplug" {
			shadowFree = true
		}
	}
	if !shadowFree {
		t.Fatalf("workspace def did not shadow the plugin's same-name agent: %+v", startupDefs)
	}
}
