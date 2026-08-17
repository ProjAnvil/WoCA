package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/settings"
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
