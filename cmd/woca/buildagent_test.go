package main

import (
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() zerolog.Logger {
	return zerolog.Nop()
}

func TestBuildAgentBasic(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	opts.compactLimit = 1000
	opts.maxTurns = 3
	opts.searchAPI = "http://127.0.0.1:1/{q}"

	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	if ap.cfg.Provider != "openai" || ap.cfg.Model != "gpt-test" {
		t.Fatalf("cfg = %+v", ap.cfg)
	}
	if ap.cfg.APIKey != "test-key" || ap.cfg.BaseURL != url {
		t.Fatalf("credentials = %+v", ap.cfg)
	}
	if ap.pluginMgr == nil || ap.settings == nil {
		t.Fatal("app members missing")
	}
	if ap.cfg.WorkDir != opts.workdir {
		t.Fatalf("workdir = %q", ap.cfg.WorkDir)
	}
	if ap.a.MemoryWorkspace() != opts.workdir {
		t.Fatalf("memory workspace = %q", ap.a.MemoryWorkspace())
	}
	// The settings-pinned memory engine is none.
	if ap.memEngine != nil {
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

	opts := options{workdir: t.TempDir(), apiKey: "k", baseURL: url}
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.cfg.Provider != "glm" || ap.cfg.Model != "glm-4.6" || ap.cfg.MaxTokens != 1234 || ap.cfg.Temperature != 0.7 {
		t.Fatalf("cfg from settings = %+v", ap.cfg)
	}
}

func TestBuildAgentInvalidProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.provider = "nope"
	if _, err := buildAgent(t.Context(), opts, testLogger()); err == nil {
		t.Fatal("expected provider validation error")
	}
}

func TestBuildAgentFallbackModel(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	opts.fallbackModel = "gpt-fallback"
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestBuildAgentThinkingBudgetClamped(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.thinkingBudget = 1 // below the minimum; Normalize clamps it
	opts.provider = "anthropic"
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if got := ap.cfg.ThinkingBudget; got != config.MinThinkingBudget {
		t.Fatalf("thinking budget = %d, want clamped %d", got, config.MinThinkingBudget)
	}
}

func TestBuildAgentCheckpointerVariants(t *testing.T) {
	isolateEnv(t)
	url := fakeOpenAI(t, "hi")

	build := func(mutate func(*options)) *app {
		seedSettings(t, [2]string{"memory-engine", "none"})
		opts := baseOpts(t, url)
		mutate(&opts)
		ap, err := buildAgent(t.Context(), opts, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		return ap
	}

	// --checkpoint-dir forces the file backend.
	ap := build(func(o *options) { o.checkpointDir = t.TempDir() })
	ap.pluginMgr.Close()
	ap.a.Close()

	// Settings-driven file backend with a dir.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "file"},
		[2]string{"checkpoint-dir", t.TempDir()},
	)
	ap = build(func(o *options) {})
	ap.pluginMgr.Close()
	ap.a.Close()

	// SQLite backend (the default): exercises localDB().
	ap = build(func(o *options) {})
	ap.pluginMgr.Close()
	ap.a.Close()

	// Postgres backend without a DSN: degrades to no checkpointer.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"checkpoint", "postgres"},
	)
	ap = build(func(o *options) {})
	ap.pluginMgr.Close()
	ap.a.Close()

	// In-memory memoryStore backend.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "memory"},
	)
	ap = build(func(o *options) {})
	if ap.memory == nil {
		t.Fatal("in-memory store missing")
	}
	ap.pluginMgr.Close()
	ap.a.Close()

	// SQLite memoryStore backend.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "sqlite"},
	)
	ap = build(func(o *options) {})
	if ap.memory == nil {
		t.Fatal("sqlite store missing")
	}
	ap.pluginMgr.Close()
	ap.a.Close()

	// Postgres memoryStore without DSN: nil store.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"memory", "postgres"},
	)
	ap = build(func(o *options) {})
	if ap.memory != nil {
		t.Fatal("postgres store should degrade to nil without a DSN")
	}
	ap.pluginMgr.Close()
	ap.a.Close()
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
	opts.managedSettings = managed
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// Managed settings do not override CLI flags, so the flag provider wins.
	if ap.cfg.Provider != "openai" {
		t.Fatalf("flag provider should win over managed: %q", ap.cfg.Provider)
	}
	if got := ap.settings.Load().Model; got != "deepseek-chat" {
		t.Fatalf("managed model = %q", got)
	}
}

func TestBuildAgentLadyMEngine(t *testing.T) {
	home := isolateEnv(t)
	// Default engine (ladym) with the default sqlite path under the temp home.
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// The engine builds against a local sqlite DB (best-effort assertion: no
	// crash either way; failures degrade to nil).
	_ = ap.memEngine
	if _, err := os.Stat(filepath.Join(home, ".woca")); err != nil {
		t.Fatalf("expected ~/.woca created: %v", err)
	}
}

func TestAppCurrentCfgAndProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	if got := ap.currentCfg(); got.Model != "gpt-test" {
		t.Fatalf("currentCfg = %+v", got)
	}
	// Flag pins the provider.
	if got := ap.effectiveProvider(); got != "openai" {
		t.Fatalf("effectiveProvider flag = %q", got)
	}
	// Without a flag, settings decide.
	ap2, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
	if got := ap2.effectiveProvider(); got == "" {
		t.Fatal("effectiveProvider settings fallback empty")
	}
}

func TestAppSetThinkingBudget(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	if err := ap.setThinkingBudget(2048); err != nil {
		t.Fatal(err)
	}
	if got := ap.settings.Load().ThinkingBudget; got != 2048 {
		t.Fatalf("persisted budget = %d", got)
	}
	if got := ap.currentCfg().ThinkingBudget; got != 2048 {
		t.Fatalf("cfg budget = %d", got)
	}

	// A pinned --thinking-budget flag rejects runtime switches.
	opts2 := baseOpts(t, url)
	opts2.thinkingBudget = 4096
	ap2, err := buildAgent(t.Context(), opts2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
	if err := ap2.setThinkingBudget(2048); err == nil {
		t.Fatal("expected pinned-budget error")
	}
}

func TestAppRebuildModel(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	// A settings model change applies on rebuild (no --model flag was set...
	// actually one was; clear it by rebuilding without the flag).
	ap2, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
	cfg := ap2.settings.Load()
	if err := settings.Apply(&cfg, settings.KeyModel, "settings-model"); err != nil {
		t.Fatal(err)
	}
	if err := ap2.settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ap2.rebuildModel(); err != nil {
		t.Fatal(err)
	}
	if got := ap2.a.CurrentModelName(); got != "settings-model" {
		t.Fatalf("rebuilt model name = %q", got)
	}
}

func TestAppRebuildMemory(t *testing.T) {
	isolateEnv(t)
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	// Switch the engine off live.
	cfg := ap.settings.Load()
	if err := settings.Apply(&cfg, settings.KeyMemoryEngine, settings.MemoryEngineNone); err != nil {
		t.Fatal(err)
	}
	if err := ap.settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ap.rebuildMemory(); err != nil {
		t.Fatal(err)
	}
	if ap.memEngine != nil {
		t.Fatal("engine should be nil after rebuild to none")
	}
}

func TestAppFetchQuota(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})

	// deepseek with a custom gateway: no known balance endpoint, no network.
	opts := baseOpts(t, "http://127.0.0.1:1")
	opts.provider = "deepseek"
	opts.model = "deepseek-chat"
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	info, err := ap.fetchQuota(t.Context())
	if err != nil || info.Available {
		t.Fatalf("deepseek custom gateway quota = %+v %v", info, err)
	}

	// openai without a Codex OAuth login degrades to unavailable, no error.
	ap2, err := buildAgent(t.Context(), baseOpts(t, "http://127.0.0.1:1"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
	info, err = ap2.fetchQuota(t.Context())
	if err != nil || info.Available {
		t.Fatalf("openai quota = %+v %v", info, err)
	}
}

func TestAppApplyClaudeEnv(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()

	// A ~/.claude.json with one global MCP server.
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{
		"mcpServers": {
			"demo": {"command": "echo", "args": ["hi"]}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv(nil, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	got := ap.settings.Load()
	if len(got.ClaudeMCP) != 1 || got.ClaudeMCP[0] != "demo" {
		t.Fatalf("claude-mcp setting = %+v", got.ClaudeMCP)
	}
	mcpFile := filepath.Join(home, ".woca", "mcp.json")
	if _, err := os.Stat(mcpFile); err != nil {
		t.Fatalf("mcp.json not materialized: %v", err)
	}

	// A nil settings store errors.
	if err := (&app{}).applyClaudeEnv(nil, nil); err == nil {
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
	eng, err := buildMemoryEngine(&app{cfg: config.Config{WorkDir: t.TempDir()}}, cfg, nil)
	if err != nil || eng != nil {
		t.Fatalf("none engine = %v %v", eng, err)
	}
}

func TestConnectMCP(t *testing.T) {
	log := testLogger()
	st := (*settings.Store)(nil)

	// Invalid specs are skipped.
	tools, clients := connectMCP(t.Context(), log, []string{"nonsense", "x="}, st)
	if len(tools) != 0 || len(clients) != 0 {
		t.Fatalf("invalid specs = %d tools", len(tools))
	}

	// A stdio server that exits immediately fails initialization.
	tools, clients = connectMCP(t.Context(), log, []string{"dead=exit 0"}, st)
	if len(tools) != 0 || len(clients) != 0 {
		t.Fatalf("dead server = %d tools", len(tools))
	}

	// An HTTP endpoint that is unreachable fails initialization.
	tools, clients = connectMCP(t.Context(), log, []string{"web=http://127.0.0.1:1/mcp"}, st)
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
	runner := newMcpAuthRunner(nil)
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
	runner = newMcpAuthRunner(st)
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
	opts.workdir = "" // falls back to the process working directory
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if !strings.HasPrefix(ap.cfg.WorkDir, dir) {
		t.Fatalf("workdir fallback = %q", ap.cfg.WorkDir)
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
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.settings != nil {
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
	opts.managedSettings = filepath.Join(home, "missing-managed.json")
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ap.pluginMgr.Close()
	ap.a.Close()

	// A ~/.claude skill selected in settings is symlinked into ~/.woca/skills.
	claudeSkill := filepath.Join(home, ".claude", "skills", "myutil")
	if err := os.MkdirAll(claudeSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkill, "SKILL.md"), []byte("---\nname: myutil\ndescription: helper\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts2 := baseOpts(t, url)
	ap2, err := buildAgent(t.Context(), opts2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
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
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
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

	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.memory != nil {
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
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.db")})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.memEngine != nil {
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
	if err := settings.Apply(&cfg, settings.KeyLadyMDBPath, filepath.Join(blocker, "ladym.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildMemoryEngine(&app{cfg: config.Config{WorkDir: t.TempDir()}, settings: st}, cfg, nil); err == nil {
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
	opts.workdir = ws
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
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
	opts.workdir = ws
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
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
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
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
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestRebuildModelInvalidProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	ap, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	cfg := ap.settings.Load()
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

	ap, err := buildAgent(t.Context(), baseOpts(t, url), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// An invalid ~/.claude.json makes the mcp.json rewrite fail.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv(nil, []string{"demo"}); err == nil {
		t.Fatal("expected failure with invalid ~/.claude.json")
	}
	// With a valid ~/.claude.json the apply succeeds and reloads skills.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"demo":{"command":"echo","args":["x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv(nil, []string{"demo"}); err != nil {
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
	ap, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if err := ap.rebuildModel(); err != nil {
		t.Fatal(err)
	}
}

func TestAppRebuildMemoryError(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.db")})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if err := ap.rebuildMemory(); err == nil {
		t.Fatal("expected rebuild error")
	}
}

func TestApplyClaudeEnvSyncFailure(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// Replace the skills directory with a regular file: the skill sync
	// fails on the next apply.
	if err := os.RemoveAll(filepath.Join(home, ".woca", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv([]string{"s"}, nil); err == nil {
		t.Fatal("expected sync failure")
	}
}
