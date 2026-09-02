package agentbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/claudeenv"
	"github.com/projanvil/woca/internal/config"
	contextx "github.com/projanvil/woca/internal/context"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/plugins"
	"github.com/projanvil/woca/internal/prompt"
	"github.com/projanvil/woca/internal/searchapi"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
)

// BuildAgent constructs the configured agent (model, prompt, hooks, skills,
// MCP tools, compaction, storage backends) shared by the REPL/one-shot, the
// TUI, and the web server. Provider credentials resolve in order: CLI flag →
// environment variable → settings database (API keys encrypted at rest) →
// OAuth token.
func BuildAgent(ctx context.Context, opts Options, log zerolog.Logger) (*App, error) {
	workdir := opts.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	workdir, _ = filepath.Abs(workdir)

	mode := resolveMode(opts.Mode, opts.SkipPerms, opts.AcceptEdits, opts.ReadOnly, opts.Plan)

	st, dbSettings := openSettings(ctx, opts, log)

	cfg, err := resolveChatConfig(opts, dbSettings, st, mode, workdir)
	if err != nil {
		return nil, err
	}

	model, err := config.BuildModel(cfg)
	if err != nil {
		return nil, err
	}

	// Optional fallback model for the retry/fallback router.
	var fallbackModel language.ChatModel
	if opts.FallbackModel != "" {
		fbCfg := cfg
		fbCfg.Model = opts.FallbackModel
		fbModel, err := config.BuildModel(fbCfg)
		if err != nil {
			return nil, err
		}
		fallbackModel = fbModel
	}

	// One executor backs every process side effect below (shell and file
	// tools, hooks, MCP stdio servers, plugin runtimes). The default is the
	// local implementation; the sandboxed remote implementation slots in
	// here unchanged (design doc §8) — the sandbox session is provisioned
	// once per process and destroyed at teardown.
	var teardown []func()
	x := executor.Executor(executor.NewLocal())
	if opts.Sandbox != nil {
		sbx, err := executor.NewSandbox(*opts.Sandbox)
		if err != nil {
			return nil, fmt.Errorf("sandbox executor: %w", err)
		}
		x = sbx
		teardown = append(teardown, func() { _ = sbx.Close() })
		log.Info().Str("endpoint", opts.Sandbox.Endpoint).Msg("process side effects routed to the sandbox executor")
	}

	// Assemble the system prompt with collected context. The same builder
	// runs for every run workspace and the active model name (the agent
	// caches per workspace+model), so serve-mode sessions in other projects
	// get the environment section, git status and project memory files of
	// the directory they operate in, and swapping the model refreshes the
	// name in the prompt. Live settings are re-read per build so cache
	// invalidation (style change) picks them up.
	sysFor := func(ws, model string) string {
		sys := prompt.BuildWithStyle(ws, cfg.ExtraSystemPrompt, model, st.Load().OutputStyle)
		if c := contextx.Collect(ws); c != "" {
			sys += "\n\n" + c
		}
		if list := loadSkills(ws, st.Load().BuiltinSkills, log); len(list) > 0 {
			sys += "\n\n# Available skills\n\nInvoke with the Skill tool; the list is scoped to this workspace.\n" + skillManifest(list)
		}
		return sys
	}
	sysPrompt := sysFor(cfg.WorkDir, cfg.Model)

	hookRunner := hooks.NewRunner(loadHooksConfig(workdir), workdir, "woca", hooks.WithLogger(log))
	// Claude environment import (settings claude-skills / claude-mcp):
	// selected ~/.claude skills are symlinked into ~/.woca/skills (loadSkills
	// picks them up from there) and the selected global MCP servers are
	// materialized as ~/.woca/mcp.json specs connected below.
	if linked, err := claudeenv.SyncSkills(wocaRoot(), dbSettings.ClaudeSkills); err != nil {
		log.Warn().Err(err).Msg("claude skill import failed")
	} else if len(linked) > 0 {
		log.Info().Strs("skills", linked).Msg("imported claude skills via ~/.woca/skills")
	}
	skillList := loadSkills(workdir, st.Load().BuiltinSkills, log)
	agentDefs := loadAgents(workdir, log)
	// Plugins (marketplace-level units under ~/.woca/plugins) contribute
	// namespaced skills and subagent definitions.
	pluginList, pluginErrs := plugins.Load(plugins.DefaultRoot())
	for _, perr := range pluginErrs {
		log.Warn().Err(perr).Msg("plugin skipped")
	}
	for _, pl := range pluginList {
		skillList = append(skillList, pl.Skills...)
		agentDefs = append(agentDefs, pl.Agents...)
	}
	// MCP servers resolve as three layers: --mcp flags (highest), the
	// workspace file workDir/.woca/mcp.json (no upward walk, D5), and the
	// user ~/.woca/mcp.json kept in sync with the ~/.claude import
	// selection. Same-name servers connect once, from the winning layer.
	var userSpecs []string
	if specs, err := claudeMCPStartupSpecs(wocaRoot(), dbSettings.ClaudeMCP); err != nil {
		log.Warn().Err(err).Msg("claude mcp import failed")
	} else if len(specs) > 0 {
		log.Info().Strs("servers", specs).Msg("imported claude mcp servers via ~/.woca/mcp.json")
		userSpecs = specs
	}
	wsSpecs, err := claudeenv.LoadMCPConfig(filepath.Join(workdir, ".woca"))
	if err != nil {
		log.Warn().Err(err).Msg("workspace mcp.json unreadable")
	}
	extraTools, mcpClients := connectMCP(ctx, log, mergeMCPSpecs(opts.MCPServers, wsSpecs, userSpecs, log), st, x)

	// Built-in capability plugins (browser via pinchtab, python/pdf via uv):
	// tools register when enabled in settings; runtimes install on demand
	// from the web settings UI. They share the plugin root (~/.woca/plugins)
	// with marketplace skills/agents plugins: shared runtimes live under
	// runtimes/, per-plugin python envs under <name>/ (WOCA_PLUGIN_ROOT
	// overrides for testing).
	pluginHome := plugins.DefaultRoot()
	if env := os.Getenv("WOCA_PLUGIN_ROOT"); env != "" {
		pluginHome = env
	}
	pluginMgr := pluginrt.NewManagerWithExecutor(pluginHome, cfg.WorkDir, log, x)
	extraTools = append(extraTools, pluginMgr.Tools(dbSettings.BuiltinPlugins)...)

	reg := tools.NewRegistryWithExecutor(cfg.WorkDir, x)
	if opts.SearchAPI != "" {
		sc := searchapi.New(searchapi.Config{Endpoint: opts.SearchAPI, APIKey: opts.SearchAPIKey})
		reg.Register(tools.WebSearchToolWithBackend(func(ctx context.Context, q string) (string, error) {
			results, err := sc.Search(ctx, q)
			if err != nil {
				return "", err
			}
			return searchapi.Format(results), nil
		}))
	}

	checkpointer, memory := openStorageBackends(ctx, opts, dbSettings, st, log)

	// Factual-memory engine (ladyM): startup build degrades to nil on
	// failure so the harness always comes up; settings changes rebuild it
	// live via app.RebuildMemory.
	memEngine, memErr := buildMemoryEngine(&App{Cfg: cfg, Settings: st}, dbSettings, model, opts.EnterpriseMemory)
	if memErr != nil {
		log.Warn().Err(memErr).Msg("memory engine unavailable; continuing without memory")
		memEngine = nil
	}

	// Max-output-tokens escalation for the recovery path (an 8k→64k retry):
	// wire the escalator only when the user did not set an explicit cap, i.e.
	// only under the default-cap condition. BuildModel keeps the thinking
	// budget enabled and the cap above it when linked.
	var escalator func() (language.ChatModel, error)
	if opts.MaxTokens == 0 && dbSettings.MaxTokens == 0 {
		escCfg := cfg
		escCfg.MaxTokens = agent.EscalatedMaxTokens
		escalator = func() (language.ChatModel, error) {
			return config.BuildModel(escCfg)
		}
	}

	// Observability log (default off): the flag forces it on; otherwise the
	// persisted debug-log setting decides. The store itself gates writes, so
	// it can be toggled live from the web settings UI or /debug-log.
	obsRoot := ""
	if home, err := os.UserHomeDir(); err == nil {
		obsRoot = filepath.Join(home, ".woca", "logs")
	}
	obs := obslog.New(obsRoot, opts.DebugLog || dbSettings.DebugLog)
	if opts.DebugLog {
		// The flag forces the log on for the whole process: dynamic refreshes
		// from the settings database cannot turn it back off.
		obs.LockOpen()
	}

	a, err := agent.New(agent.Config{
		Model:           model,
		FallbackModel:   fallbackModel,
		ModelMaxRetries: 2,
		Registry:        reg,
		Exec:            x,
		SystemPrompt:    sysPrompt,
		SystemPromptFor: sysFor,
		PermissionMode:  cfg.PermissionMode,
		RecursionLimit:  cfg.RecursionLimit,
		WorkDir:         cfg.WorkDir,
		Hooks:           hookRunner,
		HooksFor: func(ws string) *hooks.Runner {
			return hooks.NewRunner(loadHooksConfig(ws), ws, "woca", hooks.WithLogger(log))
		},
		Skills:             skillList,
		SkillsFor:          func(ws string) []skills.Skill { return loadSkills(ws, st.Load().BuiltinSkills, log) },
		ExtraTools:         extraTools,
		CompactThreshold:   opts.CompactLimit,
		Summarizer:         model,
		Permissions:        loadPermissions(workdir),
		Agents:             agentDefs,
		AgentsFor:          func(ws string) []agents.Definition { return loadAgents(ws, log) },
		CheckpointDir:      opts.CheckpointDir,
		Checkpointer:       checkpointer,
		BashPolicy:         tools.NewCommandPolicy(opts.AllowCommands, opts.DenyCommands),
		MaxTokensEscalator: escalator,
		// Tool-use summaries are a UI feature: emit them for interactive
		// sessions (REPL/TUI/serve), not for one-shot --prompt runs —
		// the non-interactive-session gate.
		EmitToolUseSummaries: opts.Prompt == "",
		// Between-turn tool refresh (stage 28): re-list MCP tools so
		// hot-added ones are usable in the next round, plus re-evaluate the
		// enabled built-in plugins (settings toggles apply live).
		RefreshTools: func(ctx context.Context) []coretools.Tool {
			fresh := refreshMCPTools(ctx, log, mcpClients)
			return append(fresh, pluginMgr.Tools(st.Load().BuiltinPlugins)...)
		},
		// Long-term memory attachments: prefetch relevant entries per turn.
		Memory:       memory,
		MemoryEngine: memEngine,
		// Live settings for the memory-engine paths (top-k, autoretain…);
		// Store.Load is nil-safe so a failed settings open keeps working.
		LoadSettings: st.Load,
		MaxTurns:     opts.MaxTurns,
		Obs:          obs,
	})
	if err != nil {
		return nil, err
	}
	a.SetMemoryWorkspace(cfg.WorkDir)
	return &App{Agent: a, Cfg: cfg, Opts: opts, Settings: st, Memory: memory, MemEngine: memEngine, Workdir: workdir, PluginMgr: pluginMgr, Log: log, Teardown: teardown}, nil
}

// wocaRoot returns ~/.woca (best-effort; ".woca" relative when the home
// directory can't be resolved).
func wocaRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".woca"
	}
	return filepath.Join(home, ".woca")
}

// managedSettingsPath resolves the enterprise managed-settings file: flag →
// env → /etc/woca/managed-settings.json when present.
func managedSettingsPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("WOCA_MANAGED_SETTINGS"); v != "" {
		return v
	}
	if _, err := os.Stat("/etc/woca/managed-settings.json"); err == nil {
		return "/etc/woca/managed-settings.json"
	}
	return ""
}

// openBrowser opens url in the default browser (best-effort).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform %q", runtime.GOOS)
	}
	return cmd.Start()
}

// OpenBrowser opens url in the default browser unless WOCA_NO_BROWSER is
// set (shared by serve, the console /web command, and the MCP OAuth flows).
func OpenBrowser(url string) {
	if os.Getenv("WOCA_NO_BROWSER") == "" {
		_ = openBrowser(url)
	}
}

// resolveMode maps the permission flags to a permission mode.
func resolveMode(mode string, skipPerms, acceptEdits, readOnly, plan bool) config.PermissionMode {
	switch {
	case skipPerms:
		return config.ModeBypass
	case acceptEdits:
		return config.ModeAcceptEdits
	case readOnly:
		return config.ModeReadOnly
	case plan:
		return config.ModePlan
	}
	if mode != "" {
		return config.PermissionMode(mode)
	}
	return config.ModeDefault
}

// resolveKey resolves the provider API key in order: CLI flag → environment
// variable → settings database (encrypted) → OAuth token.
func ResolveKey(flagKey, provider string, st *settings.Store) string {
	if flagKey != "" {
		return flagKey
	}
	switch provider {
	case "openai":
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretOpenAIAPIKey); v != "" {
			return v
		}
	case "glm":
		if v := os.Getenv("GLM_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretGLMAPIKey); v != "" {
			return v
		}
	case "deepseek":
		if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretDeepSeekAPIKey); v != "" {
			return v
		}
	default:
		if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretAnthropicAPIKey); v != "" {
			return v
		}
	}
	// Fall back to the OAuth token saved by `woca login`.
	return loadOAuthAccessToken()
}

// loadOAuthAccessToken reads the access_token from ~/.woca/credentials.json.
func loadOAuthAccessToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".woca", "credentials.json"))
	if err != nil {
		return ""
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return ""
	}
	return tok.AccessToken
}

// resolveBaseURL resolves the provider base URL in order: CLI flag →
// environment variable → settings database.
func ResolveBaseURL(flagBaseURL, provider string, dbSettings settings.Settings) string {
	if flagBaseURL != "" {
		return flagBaseURL
	}
	switch provider {
	case "openai":
		if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
			return v
		}
		return dbSettings.OpenAIBaseURL
	case "glm":
		if v := os.Getenv("GLM_BASE_URL"); v != "" {
			return v
		}
		if v := dbSettings.GLMBaseURL; v != "" {
			return v
		}
		return config.ProviderDefaultBaseURL(config.ProviderGLM)
	case "deepseek":
		if v := os.Getenv("DEEPSEEK_BASE_URL"); v != "" {
			return v
		}
		if v := dbSettings.DeepSeekBaseURL; v != "" {
			return v
		}
		return config.ProviderDefaultBaseURL(config.ProviderDeepSeek)
	default:
		if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" {
			return v
		}
		if v := os.Getenv("ANTHROPIC_API_URL"); v != "" {
			return v
		}
		return dbSettings.AnthropicBaseURL
	}
}

// claudeMCPStartupSpecs returns the MCP specs to connect at agent start: a
// non-empty selection re-materializes ~/.woca/mcp.json from ~/.claude.json;
// an empty selection keeps the previously materialized file (if any), so
// agent start is always driven by the local copy under ~/.woca.
func claudeMCPStartupSpecs(root string, selected []string) ([]string, error) {
	if len(selected) > 0 {
		return syncMCPConfig(root, selected)
	}
	return claudeenv.LoadMCPConfig(root)
}

// syncMCPConfig filters ~/.claude.json's global servers down to the selection
// and always rewrites ~/.woca/mcp.json — unchecking everything clears it.
func syncMCPConfig(root string, selected []string) ([]string, error) {
	all, err := claudeenv.Servers()
	if err != nil {
		return nil, err
	}
	keep := make([]claudeenv.MCPServer, 0, len(selected))
	for _, s := range all {
		for _, name := range selected {
			if s.Name == name {
				keep = append(keep, s)
				break
			}
		}
	}
	if err := claudeenv.WriteMCPConfig(root, keep); err != nil {
		return nil, err
	}
	return claudeenv.LoadMCPConfig(root)
}

// ApplyClaudeEnv persists a claude-skills / claude-mcp checkbox selection,
// syncs the ~/.woca skill symlinks and mcp.json, and live-applies the skill
// set. MCP servers (re)connect at the next agent start.
func (ap *App) ApplyClaudeEnv(skills, mcp []string) error {
	if ap.Settings == nil {
		return fmt.Errorf("settings store unavailable")
	}
	cfg := ap.Settings.Load()
	if err := settings.Apply(&cfg, settings.KeyClaudeSkills, settings.FormatNameList(skills)); err != nil {
		return err
	}
	if err := settings.Apply(&cfg, settings.KeyClaudeMCP, settings.FormatNameList(mcp)); err != nil {
		return err
	}
	if err := ap.Settings.Save(cfg); err != nil {
		return err
	}
	root := wocaRoot()
	if _, err := claudeenv.SyncSkills(root, skills); err != nil {
		return err
	}
	// Always rewrite mcp.json so unchecking everything disconnects the servers
	// at the next start.
	if _, err := syncMCPConfig(root, mcp); err != nil {
		return err
	}
	// Live skill reload (mirrors BuildAgent's load + plugin merge).
	list := loadSkills(ap.Workdir, ap.Settings.Load().BuiltinSkills, ap.Log)
	for _, pl := range pluginListOrEmpty() {
		list = append(list, pl.Skills...)
	}
	ap.Agent.SetSkills(list)
	return nil
}

// pluginListOrEmpty loads marketplace plugins, swallowing errors (a broken
// plugin tree must not break a skill reload).
func pluginListOrEmpty() []plugins.Plugin {
	list, _ := plugins.Load(plugins.DefaultRoot())
	return list
}

// McpAuthRunner builds the OAuth runner shared by the McpAuth tool and
// `woca mcp auth`: tokens persist encrypted in the secrets table.
func McpAuthRunner(st *settings.Store) func(ctx context.Context, server string) error {
	return func(ctx context.Context, server string) error {
		if st == nil {
			return fmt.Errorf("settings store unavailable; run `woca mcp auth %s <endpoint>` with the database enabled", server)
		}
		for _, spec := range os.Args {
			_, command, ok := strings.Cut(spec, "=")
			if !ok {
				continue
			}
			if name, _, found := strings.Cut(spec, "="); found && name != server {
				continue
			}
			if !strings.HasPrefix(command, "http://") && !strings.HasPrefix(command, "https://") {
				continue
			}
			flow := mcp.OAuthFlow{
				ServerName: server,
				Endpoint:   command,
				Client:     &http.Client{Timeout: 15 * time.Second},
				OpenBrowser: func(u string) error {
					OpenBrowser(u)
					return nil
				},
				SaveToken: func(token string) error {
					return st.SetSecret(mcp.McpTokenSecretPrefix+server+"_token", token)
				},
			}
			return mcp.RunOAuth(ctx, flow)
		}
		return fmt.Errorf("MCP server %q is not configured with an https endpoint (start woca with --mcp %s=https://…)", server, server)
	}
}

// checkpointsDefaultPath is the default on-disk checkpoint database beside
// the settings database (~/.woca/checkpoints.db).
func checkpointsDefaultPath() string {
	return filepath.Join(filepath.Dir(wocadb.DefaultPath()), "checkpoints.db")
}

// dsnWithLgcpSchema points a postgres DSN's search_path at the lgcp schema
// (URL and key=value forms both accepted; pgx treats the parameter as a
// runtime param applied to every pooled session).
func dsnWithLgcpSchema(dsn string) string {
	const param = "search_path=lgcp"
	if strings.Contains(dsn, "://") {
		if strings.Contains(dsn, "?") {
			return dsn + "&" + param
		}
		return dsn + "?" + param
	}
	return dsn + " " + param
}
