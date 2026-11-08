package agentbuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
)

// App bundles the assembled runtime: the agent, its effective config, the
// startup flags (for re-resolution), the persistent settings store, the
// memoryStore, and the factual-memory engine (ladyM; nil when disabled or
// unavailable). memMu guards the MemEngine swap in RebuildMemory; the
// agent's own engine pointer carries its own lock.
type App struct {
	Agent     *agent.Agent
	Cfg       config.Config
	Opts      Options
	Settings  *settings.Store
	Memory    *store.Store
	PluginMgr *pluginrt.Manager
	// Workdir is the resolved (absolute) working directory the agent runs
	// in; skill reloads after a claude-env selection change re-read from it.
	Workdir string
	// Log is the process logger, used by post-build reloads
	// (ApplyClaudeEnv) for merge-conflict lines.
	Log zerolog.Logger
	// Teardown holds build-time resource releases (e.g. destroying the
	// sandbox session a sandboxed executor provisioned). Entry points run
	// them at shutdown, after the agent and plugin manager close.
	Teardown []func()

	memMu     sync.Mutex
	MemEngine memory.Engine
	// cfgMu guards the Cfg copy against runtime mutations (thinking-level
	// switches) racing snapshot readers (CfgLive, RebuildModel).
	cfgMu sync.RWMutex
}

// CurrentCfg returns a copy of the effective configuration under the lock.
func (ap *App) CurrentCfg() config.Config {
	ap.cfgMu.RLock()
	defer ap.cfgMu.RUnlock()
	return ap.Cfg
}

// BaseServerConfig returns the shared base for every server.Config assembly
// (arch-cleanup 2026-11-08 F2): the fields the two former hand-built
// construction sites — the TUI /web embedded UI (cmd/woca) and the serve
// deployment form (internal/servefront) — set identically. Both start here
// and override only their form specifics, so a new shared field cannot drift
// between the two web surfaces again.
//
// Fields owned by the base: Agent, Logger, SessionDir, WorkDir, Settings,
// Memory, MemoryEngine, RebuildMemory, RebuildModel, CfgLive,
// SetThinkingBudget, Quota, ApplyClaudeEnv, Plugins, InstallPlugin, and
// ActivityAPI.
//
// ActivityAPI defaults to true: the personal single-process posture serves
// the session-activity surface from this process's agent. A deployment form
// whose sessions run elsewhere (queue dispatch) overrides it to false so the
// activity endpoints answer 501 instead of an empty panel.
//
// Fields deliberately NOT set here, with their owners:
//   - Static: each form wires its own frontend (the TUI and the personal
//     serve command embed webdist; headless API roles serve none). agentbuild
//     must not import webdist — headless roles (woca-scheduler, woca-worker)
//     link this package and must stay free of the chat UI.
//   - SessionDir's WOCA_SESSION_DIR override: servefront's shared-session
//     deployments resolve it; the base keeps the personal default
//     (<session.DefaultDir>/sessions), which servefront overrides.
//   - The serve-form specifics (Auth, Authz, NativeAuth, RunLedger,
//     DispatchToQueue, RunsOffHost, Sandbox*, WorkspaceRoot, MsgPlane,
//     GrafanaURL, FS, Audit, Extras, AnonymousAPIPaths,
//     HostGateExemptPrefixes, StreamReauth, ModelCatalog, ResolveModel):
//     servefront layers these over the base.
//
// Call after BuildAgent has fully assembled the App.
func (ap *App) BaseServerConfig() server.Config {
	sessionDir := ""
	if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	return server.Config{
		Agent:        ap.Agent,
		Logger:       ap.Log,
		SessionDir:   sessionDir,
		WorkDir:      ap.Cfg.WorkDir,
		Settings:     ap.Settings,
		Memory:       ap.Memory,
		MemoryEngine: ap.MemEngine,
		RebuildMemory: func() error {
			return ap.RebuildMemory()
		},
		RebuildModel: func() error {
			return ap.RebuildModel()
		},
		CfgLive:           func() config.Config { return ap.CurrentCfg() },
		SetThinkingBudget: ap.SetThinkingBudget,
		Quota:             ap.FetchQuota,
		ApplyClaudeEnv:    ap.ApplyClaudeEnv,
		Plugins: func(enabled []string) []pluginrt.Status {
			return ap.PluginMgr.Statuses(enabled)
		},
		InstallPlugin: func(ctx context.Context, name string, reinstall bool) error {
			if reinstall {
				return ap.PluginMgr.Reinstall(ctx, name)
			}
			return ap.PluginMgr.Install(ctx, name)
		},
		// Session activity surface: on by default — the personal posture
		// serves it from this process's agent (see the doc comment).
		ActivityAPI: true,
	}
}

// sharedBaseConfigFields is the canonical list of server.Config fields the
// shared base owns (see BaseServerConfig). It lives here, next to the base
// literal it mirrors, so the list cannot drift from the guards that pin it:
// agentbuild's TestBaseServerConfigFieldCoverage (which fails when the base
// literal and this list disagree) and servefront's cross-form consistency
// test both consume it via SharedBaseConfigFields. Static is deliberately
// absent (each form wires its own frontend); MemoryEngine and SessionDir may
// legitimately hold their zero value.
var sharedBaseConfigFields = []string{
	"Agent", "Logger", "SessionDir", "WorkDir", "Settings", "Memory",
	"MemoryEngine", "RebuildMemory", "RebuildModel", "CfgLive",
	"SetThinkingBudget", "Quota", "ApplyClaudeEnv", "Plugins",
	"InstallPlugin", "ActivityAPI",
}

// SharedBaseConfigFields returns the server.Config field names owned by the
// shared base (BaseServerConfig) — the set every deployment form must agree
// on (arch-cleanup 2026-11-08 F2). The returned slice is a copy; iterating
// it is the supported use, and mutating it does not affect the base.
func SharedBaseConfigFields() []string {
	return slices.Clone(sharedBaseConfigFields)
}

// effectiveProvider resolves the provider the same way RebuildModel does:
// startup flag first, then the settings database.
func (ap *App) effectiveProvider() string {
	if ap.Opts.Provider != "" {
		return ap.Opts.Provider
	}
	return ap.Settings.Load().Provider
}

// SetThinkingBudget persists the extended-thinking budget to the settings
// database, updates the effective config, and rebuilds the chat model so the
// change applies from the next model call. A --thinking-budget flag pins the
// value; runtime switches are rejected in that case.
func (ap *App) SetThinkingBudget(budget int) error {
	if ap.Opts.ThinkingBudget != 0 {
		return fmt.Errorf("thinking budget is pinned by the --thinking-budget flag (restart without it to switch at runtime)")
	}
	cfg := ap.Settings.Load()
	if err := settings.Apply(&cfg, settings.KeyThinkingBudget, strconv.Itoa(budget)); err != nil {
		return err
	}
	if err := ap.Settings.Save(cfg); err != nil {
		return err
	}
	ap.cfgMu.Lock()
	ap.Cfg.ThinkingBudget = budget
	ap.cfgMu.Unlock()
	return ap.RebuildModel()
}

// RebuildModel reconstructs the chat model from the current effective
// configuration (startup flags + live settings from the database) and swaps
// it into the agent. Used by the settings UIs after provider, model, base
// URL, or API key changes. The model id is re-resolved from settings (a
// startup --model flag still wins), so the settings UI's model switch takes
// effect live — and, through the name-keyed prompt cache, refreshes the
// model named in the system prompt.
func (ap *App) RebuildModel() error {
	dbSettings := ap.Settings.Load()
	provider := ap.effectiveProvider()
	cfg := ap.CurrentCfg()
	cfg.Provider = config.Provider(provider)
	cfg.APIKey = ResolveKey(ap.Opts.APIKey, provider, ap.Settings)
	cfg.BaseURL = ResolveBaseURL(ap.Opts.BaseURL, provider, dbSettings)
	if v := dbSettings.Model; v != "" && ap.Opts.Model == "" {
		cfg.Model = v
	}
	if v := dbSettings.MaxTokens; v != 0 && ap.Opts.MaxTokens == 0 {
		cfg.MaxTokens = v
	}
	if v := dbSettings.Temperature; v != 0 && ap.Opts.Temperature == 0 {
		cfg.Temperature = v
	}
	m, err := config.BuildModel(cfg)
	if err != nil {
		return err
	}
	return ap.Agent.SetModel(m)
}

// memoryCloseGrace is how long a superseded memory engine stays open after a
// rebuild so in-flight prefetch/tool calls on it degrade to a logged error
// instead of racing a Close.
const memoryCloseGrace = 60 * time.Second

// RebuildMemory reconstructs the memory engine from live settings and swaps
// it into the agent. The old engine closes after a grace period so in-flight
// prefetch/tool calls on it degrade to a logged error instead of racing a
// Close.
func (ap *App) RebuildMemory() error {
	dbSettings := ap.Settings.Load()
	eng, err := buildMemoryEngine(ap, dbSettings, ap.Agent.CurrentModel(), false)
	if err != nil {
		return err
	}
	ap.memMu.Lock()
	old := ap.MemEngine
	ap.MemEngine = eng
	ap.memMu.Unlock()
	ap.Agent.SetMemoryEngine(eng)
	if old != nil {
		go func() {
			time.Sleep(memoryCloseGrace)
			_ = old.Close()
		}()
	}
	return nil
}

// FetchQuota queries the provider's account usage (the provider-template
// dispatch: GLM Coding Plan monitor, DeepSeek balance, official OAuth
// subscriptions) with the credentials the chat model uses.
func (ap *App) FetchQuota(ctx context.Context) (quota.Info, error) {
	dbSettings := ap.Settings.Load()
	provider := ap.effectiveProvider()
	return quota.Fetch(ctx, provider,
		ResolveBaseURL(ap.Opts.BaseURL, provider, dbSettings),
		ResolveKey(ap.Opts.APIKey, provider, ap.Settings),
		nil)
}

// buildMemoryEngine constructs the memory engine from current settings.
// engine=none returns (nil, nil); build failures degrade to nil (logged by
// the caller) so woca always starts.
func buildMemoryEngine(ap *App, dbSettings settings.Settings, chatModel language.ChatModel, enterprise bool) (memory.Engine, error) {
	if dbSettings.MemoryEngine == settings.MemoryEngineNone {
		return nil, nil
	}
	if dbSettings.MemoryEngine == settings.MemoryEngineRemote {
		// The ladyM service address and its data-plane credentials are
		// infrastructure config (design §12.2): environment values win over
		// the settings rows so deployments wire the endpoint without
		// seeding the database, while personal editions keep using settings.
		rcfg := memory.RemoteConfig{
			Endpoint: dbSettings.LadyMEndpoint,
			Username: dbSettings.LadyMAPIUsername,
			Password: ap.Settings.GetSecret(settings.SecretLadyMAPIPassword),
		}
		if v := os.Getenv("WOCA_LADYM_ENDPOINT"); v != "" {
			rcfg.Endpoint = v
		}
		if v := os.Getenv("WOCA_LADYM_API_USERNAME"); v != "" {
			rcfg.Username = v
		}
		if v := os.Getenv("WOCA_LADYM_API_PASSWORD"); v != "" {
			rcfg.Password = v
		}
		return memory.BuildRemote(rcfg)
	}
	if enterprise {
		// The embedded engine keeps its database inside this process; a
		// multi-tenant deployment must not mix tenant memory into one local
		// store. Only the remote ladyM enterprise API (or none) is allowed.
		return nil, fmt.Errorf("enterprise deployments require memory-engine remote (with ladym-endpoint) or none; the embedded ladyM engine is not available")
	}
	dbPath := memory.ExpandHome(dbSettings.LadyMDBPath)
	if dbPath == "" {
		dbPath = memory.DefaultDBPath()
	}
	in := memory.BuildInput{
		EngineEnabled:    true,
		DBPath:           dbPath,
		DefaultWorkspace: ap.Cfg.WorkDir,
		Embedding:        dbSettings.LadyMEmbedding,
		EmbeddingModel:   dbSettings.LadyMEmbeddingModel,
		EmbeddingBaseURL: dbSettings.LadyMEmbeddingBaseURL,
		EmbeddingAPIKey:  ap.Settings.GetSecret(settings.SecretLadyMEmbeddingAPIKey),
		LLM:              dbSettings.LadyMLLM,
		ChatModel:        chatModel,
		OpenAIKey:        ap.Settings.GetSecret(settings.SecretOpenAIAPIKey),
		OpenAIBaseURL:    dbSettings.OpenAIBaseURL,
	}
	eng, err := memory.Build(in)
	if err != nil {
		return nil, err
	}
	return eng, nil
}
