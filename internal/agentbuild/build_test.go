package agentbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

func testLogger() zerolog.Logger {
	return zerolog.Nop()
}

func TestBuildAgentBasic(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	opts.CompactLimit = 1000
	opts.MaxTurns = 3
	opts.SearchAPI = "http://127.0.0.1:1/{q}"

	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	if ap.Cfg.Provider != "openai" || ap.Cfg.Model != "gpt-test" {
		t.Fatalf("cfg = %+v", ap.Cfg)
	}
	if ap.Cfg.APIKey != "test-key" || ap.Cfg.BaseURL != url {
		t.Fatalf("credentials = %+v", ap.Cfg)
	}
	if ap.PluginMgr == nil || ap.Settings == nil {
		t.Fatal("app members missing")
	}
	if ap.Cfg.WorkDir != opts.Workdir {
		t.Fatalf("workdir = %q", ap.Cfg.WorkDir)
	}
	if ap.Agent.MemoryWorkspace() != opts.Workdir {
		t.Fatalf("memory workspace = %q", ap.Agent.MemoryWorkspace())
	}
	// The settings-pinned memory engine is none.
	if ap.MemEngine != nil {
		t.Fatal("memory-engine=none should build no engine")
	}
}

func TestBuildAgentDefaultsFromSettings(t *testing.T) {
	home := isolateEnv(t)
	url := fakeOpenAI(t, "hi")
	// Persist provider/model/max-tokens/temperature; buildAgent with no flags
	// picks them up.
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Load()
	for _, kv := range [][2]string{
		{"memory-engine", "none"},
		{"provider", "glm"},
		{"model", "glm-4.6"},
		{"max-tokens", "1234"},
		{"temperature", "0.7"},
	} {
		if err := settings.Apply(&cfg, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	st.Close()

	opts := Options{Workdir: t.TempDir(), APIKey: "k", BaseURL: url}
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if ap.Cfg.Provider != "glm" || ap.Cfg.Model != "glm-4.6" || ap.Cfg.MaxTokens != 1234 || ap.Cfg.Temperature != 0.7 {
		t.Fatalf("cfg from settings = %+v", ap.Cfg)
	}
}

func TestBuildAgentInvalidProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.Provider = "nope"
	if _, err := BuildAgent(t.Context(), opts, testLogger()); err == nil {
		t.Fatal("expected provider validation error")
	}
}

func TestBuildAgentFallbackModel(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	opts.FallbackModel = "gpt-fallback"
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestBuildAgentThinkingBudgetClamped(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.ThinkingBudget = 1 // below the minimum; Normalize clamps it
	opts.Provider = "anthropic"
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if got := ap.Cfg.ThinkingBudget; got != config.MinThinkingBudget {
		t.Fatalf("thinking budget = %d, want clamped %d", got, config.MinThinkingBudget)
	}
}

func TestBuildAgentCheckpointerVariants(t *testing.T) {
	isolateEnv(t)
	url := fakeOpenAI(t, "hi")

	build := func(mutate func(*Options)) *App {
		seedSettings(t, [2]string{"memory-engine", "none"})
		opts := baseOpts(t, url)
		mutate(&opts)
		ap, err := BuildAgent(t.Context(), opts, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		return ap
	}

	// --checkpoint-dir forces the file backend.
	ap := build(func(o *Options) { o.CheckpointDir = t.TempDir() })
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// Settings-driven file backend with a dir.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "file"},
		[2]string{"checkpoint-dir", t.TempDir()},
	)
	ap = build(func(o *Options) {})
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// SQLite backend (the default): exercises localDB().
	ap = build(func(o *Options) {})
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// Postgres backend without a DSN: degrades to no checkpointer.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "postgres"},
	)
	ap = build(func(o *Options) {})
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// In-memory memoryStore backend.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "memory"},
	)
	ap = build(func(o *Options) {})
	if ap.Memory == nil {
		t.Fatal("in-memory store missing")
	}
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// SQLite memoryStore backend.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "sqlite"},
	)
	ap = build(func(o *Options) {})
	if ap.Memory == nil {
		t.Fatal("sqlite store missing")
	}
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// Postgres memoryStore without DSN: nil store.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "postgres"},
	)
	ap = build(func(o *Options) {})
	if ap.Memory != nil {
		t.Fatal("postgres store should degrade to nil without a DSN")
	}
	ap.PluginMgr.Close()
	ap.Agent.Close()
}

func TestBuildAgentManagedSettings(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	managed := filepath.Join(home, "managed.json")
	if err := os.WriteFile(managed, []byte(`{"provider":"deepseek","model":"deepseek-chat"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, url)
	opts.ManagedSettings = managed
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	// Managed settings do not override CLI flags, so the flag provider wins.
	if ap.Cfg.Provider != "openai" {
		t.Fatalf("flag provider should win over managed: %q", ap.Cfg.Provider)
	}
	if got := ap.Settings.Load().Model; got != "deepseek-chat" {
		t.Fatalf("managed model = %q", got)
	}
}

func TestBuildAgentLadyMEngine(t *testing.T) {
	home := isolateEnv(t)
	// Default engine (ladym) with the default sqlite path under the temp home.
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	// The engine builds against a local sqlite DB (best-effort assertion: no
	// crash either way; failures degrade to nil).
	_ = ap.MemEngine
	if _, err := os.Stat(filepath.Join(home, ".woca")); err != nil {
		t.Fatalf("expected ~/.woca created: %v", err)
	}
}

func TestAppCurrentCfgAndProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	if got := ap.CurrentCfg(); got.Model != "gpt-test" {
		t.Fatalf("currentCfg = %+v", got)
	}
	// Flag pins the provider.
	if got := ap.effectiveProvider(); got != "openai" {
		t.Fatalf("effectiveProvider flag = %q", got)
	}
	// Without a flag, settings decide.
	ap2, err := BuildAgent(t.Context(), Options{Workdir: t.TempDir(), APIKey: "k", BaseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.PluginMgr.Close()
	defer ap2.Agent.Close()
	if got := ap2.effectiveProvider(); got == "" {
		t.Fatal("effectiveProvider settings fallback empty")
	}
}

func TestAppSetThinkingBudget(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	if err := ap.SetThinkingBudget(2048); err != nil {
		t.Fatal(err)
	}
	if got := ap.Settings.Load().ThinkingBudget; got != 2048 {
		t.Fatalf("persisted budget = %d", got)
	}
	if got := ap.CurrentCfg().ThinkingBudget; got != 2048 {
		t.Fatalf("cfg budget = %d", got)
	}

	// A pinned --thinking-budget flag rejects runtime switches.
	opts2 := baseOpts(t, url)
	opts2.ThinkingBudget = 4096
	ap2, err := BuildAgent(t.Context(), opts2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.PluginMgr.Close()
	defer ap2.Agent.Close()
	if err := ap2.SetThinkingBudget(2048); err == nil {
		t.Fatal("expected pinned-budget error")
	}
}

func TestAppRebuildModel(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	// A settings model change applies on rebuild (no --model flag was set...
	// actually one was; clear it by rebuilding without the flag).
	ap2, err := BuildAgent(t.Context(), Options{Workdir: t.TempDir(), APIKey: "k", BaseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.PluginMgr.Close()
	defer ap2.Agent.Close()
	cfg := ap2.Settings.Load()
	if err := settings.Apply(&cfg, settings.KeyModel, "settings-model"); err != nil {
		t.Fatal(err)
	}
	if err := ap2.Settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ap2.RebuildModel(); err != nil {
		t.Fatal(err)
	}
	if got := ap2.Agent.CurrentModelName(); got != "settings-model" {
		t.Fatalf("rebuilt model name = %q", got)
	}
}

func TestAppRebuildMemory(t *testing.T) {
	isolateEnv(t)
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	// Switch the engine off live.
	cfg := ap.Settings.Load()
	if err := settings.Apply(&cfg, settings.KeyMemoryEngine, settings.MemoryEngineNone); err != nil {
		t.Fatal(err)
	}
	if err := ap.Settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ap.RebuildMemory(); err != nil {
		t.Fatal(err)
	}
	if ap.MemEngine != nil {
		t.Fatal("engine should be nil after rebuild to none")
	}
}

func TestAppFetchQuota(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})

	// deepseek with a custom gateway: no known balance endpoint, no network.
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.Provider = "deepseek"
	opts.Model = "deepseek-chat"
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	info, err := ap.FetchQuota(t.Context())
	if err != nil || info.Available {
		t.Fatalf("deepseek custom gateway quota = %+v %v", info, err)
	}

	// openai without a Codex OAuth login degrades to unavailable, no error.
	ap2, err := BuildAgent(t.Context(), baseOpts(t, "http://127.0.0.1:1"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.PluginMgr.Close()
	defer ap2.Agent.Close()
	info, err = ap2.FetchQuota(t.Context())
	if err != nil || info.Available {
		t.Fatalf("openai quota = %+v %v", info, err)
	}
}

func TestAppApplyClaudeEnv(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	// A ~/.claude.json with one global MCP server.
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{
		"mcpServers": {
			"demo": {"command": "echo", "args": ["hi"]}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.ApplyClaudeEnv(nil, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	got := ap.Settings.Load()
	if len(got.ClaudeMCP) != 1 || got.ClaudeMCP[0] != "demo" {
		t.Fatalf("claude-mcp setting = %+v", got.ClaudeMCP)
	}
	mcpFile := filepath.Join(home, ".woca", "mcp.json")
	if _, err := os.Stat(mcpFile); err != nil {
		t.Fatalf("mcp.json not materialized: %v", err)
	}

	// A nil settings store errors.
	if err := (&App{}).ApplyClaudeEnv(nil, nil); err == nil {
		t.Fatal("expected settings-unavailable error")
	}
}

func TestClaudeMCPStartupSpecs(t *testing.T) {
	home := isolateEnv(t)
	root := filepath.Join(home, ".woca")

	// No selection and no materialized file: no specs.
	specs, err := claudeMCPStartupSpecs(root, nil)
	if err != nil || len(specs) != 0 {
		t.Fatalf("empty startup specs = %v %v", specs, err)
	}

	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{
		"mcpServers": {
			"alpha": {"command": "echo", "args": ["a"]},
			"beta": {"command": "echo", "args": ["b"]}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	specs, err = claudeMCPStartupSpecs(root, []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || !strings.Contains(specs[0], "alpha=") {
		t.Fatalf("selected specs = %v", specs)
	}

	// Empty selection keeps the previously materialized file.
	specs, err = claudeMCPStartupSpecs(root, nil)
	if err != nil || len(specs) != 1 {
		t.Fatalf("kept specs = %v %v", specs, err)
	}

	// Unchecking everything clears the file.
	if _, err := syncMCPConfig(root, nil); err != nil {
		t.Fatal(err)
	}
	specs, err = claudeMCPStartupSpecs(root, nil)
	if err != nil || len(specs) != 0 {
		t.Fatalf("cleared specs = %v %v", specs, err)
	}
}

func TestBuildMemoryEngineNone(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyMemoryEngine, settings.MemoryEngineNone); err != nil {
		t.Fatal(err)
	}
	eng, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}}, cfg, nil, false)
	if err != nil || eng != nil {
		t.Fatalf("none engine = %v %v", eng, err)
	}
}

func TestConnectMCP(t *testing.T) {
	log := testLogger()
	st := (*settings.Store)(nil)

	// Invalid specs are skipped.
	tools, clients := connectMCP(t.Context(), log, []string{"nonsense", "x="}, st, nil)
	if len(tools) != 0 || len(clients) != 0 {
		t.Fatalf("invalid specs = %d tools", len(tools))
	}

	// A stdio server that exits immediately fails initialization.
	tools, clients = connectMCP(t.Context(), log, []string{"dead=exit 0"}, st, nil)
	if len(tools) != 0 || len(clients) != 0 {
		t.Fatalf("dead server = %d tools", len(tools))
	}

	// An HTTP endpoint that is unreachable fails initialization.
	tools, clients = connectMCP(t.Context(), log, []string{"web=http://127.0.0.1:1/mcp"}, st, nil)
	if len(tools) != 0 || len(clients) != 0 {
		t.Fatalf("unreachable http = %d tools", len(tools))
	}
}

func TestRefreshMCPTools(t *testing.T) {
	// A client whose server is gone yields no tools without panicking.
	client := mcp.NewStdio("gone", "sh", "-c", "exit 0")
	if got := refreshMCPTools(t.Context(), testLogger(), []*mcp.Client{client}); len(got) != 0 {
		t.Fatalf("refresh = %d tools", len(got))
	}
	if got := refreshMCPTools(t.Context(), testLogger(), nil); len(got) != 0 {
		t.Fatalf("nil clients = %d tools", len(got))
	}
}

func TestNewMcpAuthRunner(t *testing.T) {
	// nil store: helpful error.
	runner := McpAuthRunner(nil)
	if err := runner(t.Context(), "srv"); err == nil {
		t.Fatal("expected store-unavailable error")
	}

	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// No --mcp spec for the server in os.Args: configuration error.
	runner = McpAuthRunner(st)
	if err := runner(t.Context(), "srv"); err == nil {
		t.Fatal("expected not-configured error")
	}

	// With a spec pointing at an unreachable endpoint, the flow fails.
	oldArgs := os.Args
	os.Args = []string{"woca", "--mcp", "srv=http://127.0.0.1:1/mcp"}
	defer func() { os.Args = oldArgs }()
	if err := runner(t.Context(), "srv"); err == nil {
		t.Fatal("expected oauth failure")
	}
}

func TestBuildAgentDefaultsWorkdir(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	dir := t.TempDir()
	t.Chdir(dir)
	opts := baseOpts(t, url)
	opts.Workdir = "" // falls back to the process working directory
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if !strings.HasPrefix(ap.Cfg.WorkDir, dir) {
		t.Fatalf("workdir fallback = %q", ap.Cfg.WorkDir)
	}
}

func TestBuildAgentSettingsOpenFailure(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The database path sits under a regular file: settings.Open fails and
	// buildAgent continues without persisted settings.
	t.Setenv("WOCA_DB", filepath.Join(blocker, "woca.db"))
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if ap.Settings != nil {
		t.Fatal("settings should degrade to nil")
	}
}

func TestBuildAgentManagedUnreadableAndSkills(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-skills", `["myutil"]`},
	)
	url := fakeOpenAI(t, "hi")

	// Unreadable managed settings are warned about and ignored.
	opts := baseOpts(t, url)
	opts.ManagedSettings = filepath.Join(home, "missing-managed.json")
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ap.PluginMgr.Close()
	ap.Agent.Close()

	// A ~/.claude skill selected in settings is symlinked into ~/.woca/skills.
	claudeSkill := filepath.Join(home, ".claude", "skills", "myutil")
	if err := os.MkdirAll(claudeSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkill, "SKILL.md"), []byte("---\nname: myutil\ndescription: helper\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts2 := baseOpts(t, url)
	ap2, err := BuildAgent(t.Context(), opts2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.PluginMgr.Close()
	defer ap2.Agent.Close()
	if _, err := os.Lstat(filepath.Join(home, ".woca", "skills", "myutil")); err != nil {
		t.Fatalf("imported skill missing: %v", err)
	}
}

func TestBuildAgentBrokenPluginAndClaudeMCP(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-mcp", `["alpha"]`},
	)
	url := fakeOpenAI(t, "hi")

	// A plugin with an invalid manifest is reported and skipped.
	pluginRoot := filepath.Join(home, "plugins", "broken")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A selected claude MCP server gets materialized and connected (its
	// stdio command fails the MCP handshake, which is warned and skipped).
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{
		"mcpServers": {"alpha": {"command": "echo", "args": ["hi"]}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestBuildAgentPostgresBackends(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Load()
	for _, kv := range [][2]string{
		{"memory-engine", "none"},
		{"checkpoint", "postgres"},
		{"memory", "postgres"},
	} {
		if err := settings.Apply(&cfg, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// A postgres DSN pointing at a closed local port: opens fail fast and
	// both backends degrade to nil.
	if err := st.SetSecret(settings.SecretPostgresURL, "postgres://u:p@127.0.0.1:1/db"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if ap.Memory != nil {
		t.Fatal("postgres memory backend should degrade to nil")
	}
}

func TestBuildAgentMemoryEngineBuildFailure(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default engine (ladym) with an unusable DB path: the build fails and
	// buildAgent continues without the memory engine.
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.DB")})
	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if ap.MemEngine != nil {
		t.Fatal("broken engine path should degrade to nil")
	}
}

func TestBuildMemoryEngineBadPath(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyLadyMDBPath, filepath.Join(blocker, "ladym.DB")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}, Settings: st}, cfg, nil, false); err == nil {
		t.Fatal("expected engine build error")
	}
}

func TestBuildAgentGitRepoAndPlugin(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	// A git repository workdir makes contextx.Collect produce output.
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A valid plugin contributing one skill and one agent.
	pluginRoot := filepath.Join(home, "plugins", "toolkit")
	skillDir := filepath.Join(pluginRoot, "skills", "deploy")
	agentDir := filepath.Join(pluginRoot, "agents")
	for _, d := range []string{skillDir, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{"name":"toolkit","version":"1.0.0","description":"tools"}`
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy things\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "reviewer.md"), []byte("---\nname: reviewer\ndescription: reviews\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.Workdir = ws
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestBuildAgentWorkspaceContextAndPlugins(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A project-level skill shows up in the prompt builder's skill section.
	skillDir := filepath.Join(ws, ".woca", "skills", "localutil")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: localutil\ndescription: local helper\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plugin under ~/.woca/plugins contributes skills and agents.
	plugin := filepath.Join(home, ".woca", "plugins", "toolkit")
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), []byte(`{"name":"toolkit","version":"1.0.0","description":"tools"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy things\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.Workdir = ws
	ap, err := BuildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestBuildAgentSyncSkillsError(t *testing.T) {
	home := isolateEnv(t)
	// A claude-skills selection while ~/.woca/skills exists as a regular
	// file: the sync fails and is warned about.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-skills", `["myutil"]`},
	)
	if err := os.MkdirAll(filepath.Join(home, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestBuildAgentClaudeMCPImportError(t *testing.T) {
	home := isolateEnv(t)
	// A claude-mcp selection with an unreadable ~/.claude.json warns and
	// continues.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-mcp", `["alpha"]`},
	)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestRebuildModelInvalidProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	ap, err := BuildAgent(t.Context(), Options{Workdir: t.TempDir(), APIKey: "k", BaseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	cfg := ap.Settings.Load()
	// The settings layer rejects an unknown provider before it can ever
	// reach rebuildModel.
	if err := settings.Apply(&cfg, settings.KeyProvider, "nope"); err == nil {
		t.Fatal("expected provider validation error at Apply")
	}
}

func TestAppApplyClaudeEnvBranches(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	// A plugin with a skill so the live-reload merge loop has content.
	plugin := filepath.Join(home, ".woca", "plugins", "toolkit")
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), []byte(`{"name":"toolkit","version":"1.0.0","description":"tools"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	ap, err := BuildAgent(t.Context(), baseOpts(t, url), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	// An invalid ~/.claude.json makes the mcp.json rewrite fail.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.ApplyClaudeEnv(nil, []string{"demo"}); err == nil {
		t.Fatal("expected failure with invalid ~/.claude.json")
	}
	// With a valid ~/.claude.json the apply succeeds and reloads skills.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"demo":{"command":"echo","args":["x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.ApplyClaudeEnv(nil, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
}

func TestAppRebuildModelPicksUpLimits(t *testing.T) {
	isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"max-tokens", "4321"},
		[2]string{"temperature", "0.5"},
	)
	url := fakeOpenAI(t, "hi")
	ap, err := BuildAgent(t.Context(), Options{Workdir: t.TempDir(), APIKey: "k", BaseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if err := ap.RebuildModel(); err != nil {
		t.Fatal(err)
	}
}

func TestAppRebuildMemoryError(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.DB")})
	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	if err := ap.RebuildMemory(); err == nil {
		t.Fatal("expected rebuild error")
	}
}

func TestApplyClaudeEnvSyncFailure(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	ap, err := BuildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	// Replace the skills directory with a regular file: the skill sync
	// fails on the next apply.
	if err := os.RemoveAll(filepath.Join(home, ".woca", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.ApplyClaudeEnv([]string{"s"}, nil); err == nil {
		t.Fatal("expected sync failure")
	}
}

func TestEnterpriseMemoryForbidsEmbeddedEngine(t *testing.T) {
	cfg := settings.Default()
	// The default engine is the embedded ladyM: forbidden in enterprise mode.
	if _, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}}, cfg, nil, true); err == nil {
		t.Fatal("embedded engine allowed in enterprise mode")
	}
	// none stays allowed.
	cfgNone := cfg
	cfgNone.MemoryEngine = settings.MemoryEngineNone
	if eng, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}}, cfgNone, nil, true); err != nil || eng != nil {
		t.Fatalf("none engine = %v err=%v", eng, err)
	}
	// remote without endpoint is a configuration error, not a silent fallback.
	cfgRemote := cfg
	cfgRemote.MemoryEngine = settings.MemoryEngineRemote
	if _, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}}, cfgRemote, nil, true); err == nil {
		t.Fatal("remote engine without endpoint accepted")
	}
	// Personal mode keeps the embedded default.
	if _, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}}, cfg, nil, false); err != nil {
		t.Fatalf("personal embedded engine = %v", err)
	}
}

// The ladyM remote endpoint/credentials are infrastructure config (design
// §12.2): WOCA_LADYM_* env values must override the (possibly empty)
// settings rows so deployments wire memory without seeding the database.
func TestBuildMemoryEngineRemoteEnvFallback(t *testing.T) {
	isolateEnv(t)
	st, err := settings.Open(filepath.Join(t.TempDir(), "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyMemoryEngine, settings.MemoryEngineRemote); err != nil {
		t.Fatal(err)
	}

	// Without any endpoint the remote build must fail loudly.
	if _, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}, Settings: st}, cfg, nil, false); err == nil {
		t.Fatal("remote engine without endpoint should fail")
	}

	// With the env endpoint it builds (no settings row seeded).
	t.Setenv("WOCA_LADYM_ENDPOINT", "http://ladym-api-1:8080")
	t.Setenv("WOCA_LADYM_API_USERNAME", "woca-worker")
	t.Setenv("WOCA_LADYM_API_PASSWORD", "pw")
	eng, err := buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}, Settings: st}, cfg, nil, false)
	if err != nil || eng == nil {
		t.Fatalf("remote engine via env = %v, %v", eng, err)
	}

	// Env wins over the settings row (infra layer precedence).
	if err := settings.Apply(&cfg, settings.KeyLadyMEndpoint, "http://stale-settings:8080"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WOCA_LADYM_ENDPOINT", "http://ladym-api-2:8080")
	eng, err = buildMemoryEngine(&App{Cfg: config.Config{WorkDir: t.TempDir()}, Settings: st}, cfg, nil, false)
	if err != nil || eng == nil {
		t.Fatalf("remote engine env precedence = %v, %v", eng, err)
	}
}
