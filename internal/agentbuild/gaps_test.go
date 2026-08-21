package agentbuild

// Gap tests: checkpoint backend fallbacks, debug-log flag, plugin
// contributions, invalid MCP layer files, and helper branches.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDsnWithLgcpSchema(t *testing.T) {
	if got := dsnWithLgcpSchema("postgres://h/db"); got != "postgres://h/db?search_path=lgcp" {
		t.Fatalf("url without query = %q", got)
	}
	if got := dsnWithLgcpSchema("postgres://h/db?sslmode=disable"); got != "postgres://h/db?sslmode=disable&search_path=lgcp" {
		t.Fatalf("url with query = %q", got)
	}
	if got := dsnWithLgcpSchema("host=h db=d"); got != "host=h db=d search_path=lgcp" {
		t.Fatalf("key-value dsn = %q", got)
	}
}

func TestApplyClaudeEnvWithoutSettings(t *testing.T) {
	ap := &App{}
	if err := ap.ApplyClaudeEnv(nil, nil); err == nil {
		t.Fatal("ApplyClaudeEnv without a settings store should fail")
	}
}

func TestBuildAgentCheckpointDirUnavailable(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	opts.CheckpointDir = filepath.Join(blocker, "ckpt")
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatalf("BuildAgent should survive an unavailable checkpoint dir: %v", err)
	}
	defer ap.Agent.Close()
}

func TestBuildAgentCheckpointFileBackendWithoutDir(t *testing.T) {
	isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "file"},
	)
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Agent.Close()
}

func TestBuildAgentCheckpointPostgresWithoutDSN(t *testing.T) {
	isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "postgres"},
	)
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Agent.Close()
}

func TestBuildAgentDebugLogFlag(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	opts.DebugLog = true
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Agent.Close()
}

func TestBuildAgentInvalidWorkspaceMCPConfig(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	if err := os.MkdirAll(filepath.Join(opts.Workdir, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.Workdir, ".woca", "mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Agent.Close()
}

// TestBuildAgentPluginContributions: a plugin under the isolated root
// contributes namespaced skills and agent definitions to the build.
func TestBuildAgentPluginContributions(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// plugins.DefaultRoot is ~/.woca/plugins (not the WOCA_PLUGIN_ROOT env).
	root := filepath.Join(home, ".woca", "plugins", "helper")
	if err := os.MkdirAll(filepath.Join(root, "skills", "greet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.json"),
		[]byte(`{"name":"helper","version":"1.0","description":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "greet", "SKILL.md"),
		[]byte("---\nname: greet\ndescription: says hi\n---\nSay hi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "helper.md"),
		[]byte("---\nname: helper\ndescription: helps out\n---\nYou help out.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, fakeOpenAI(t, "ok"))
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Agent.Close()
	found := false
	for _, d := range ap.Agent.AgentDefinitions() {
		if d.Name == "helper" {
			found = true
		}
	}
	if !found {
		t.Fatal("plugin agents not contributed")
	}
}
