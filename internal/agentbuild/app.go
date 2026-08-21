package agentbuild

import (
	"context"
	"fmt"
	"os"
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
