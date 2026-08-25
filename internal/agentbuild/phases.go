package agentbuild

import (
	"context"
	"os"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

// openSettings is BuildAgent's first phase: open the settings database,
// apply the managed-settings overlay, run the edition pre-build hook (the
// bridge's blocking config pull — it must land before any setting is
// read), and load the effective settings. A missing database degrades to
// in-memory defaults (the harness must always come up).
func openSettings(ctx context.Context, opts Options, log zerolog.Logger) (*settings.Store, settings.Settings) {
	dbTarget := opts.DB
	if dbTarget == "" {
		dbTarget = os.Getenv("WOCA_DB")
	}
	st, err := settings.Open(dbTarget)
	if err != nil {
		log.Warn().Err(err).Msg("settings database unavailable; continuing without persisted settings")
		st = nil
	}
	// Enterprise managed settings: override and lock (highest precedence).
	if managed := managedSettingsPath(opts.ManagedSettings); managed != "" {
		if m, err := settings.LoadManaged(managed); err != nil {
			log.Warn().Err(err).Msg("managed settings file unreadable; ignoring")
		} else {
			st.SetManaged(m)
			log.Info().Str("path", managed).Msg("managed settings applied")
		}
	}
	if opts.OnSettingsReady != nil && st != nil {
		if err := opts.OnSettingsReady(ctx, st); err != nil {
			log.Warn().Err(err).Msg("pre-build settings hook failed; continuing")
		}
	}
	return st, st.Load()
}

// resolveChatConfig folds flags over database settings (flag wins) and
// assembles the normalized chat-model configuration — provider defaults,
// key resolution order, and the runtime-switchable thinking budget.
func resolveChatConfig(opts Options, dbSettings settings.Settings, st *settings.Store, mode config.PermissionMode, workdir string) (config.Config, error) {
	provider := opts.Provider
	if provider == "" {
		provider = dbSettings.Provider
	}
	modelName := opts.Model
	if modelName == "" {
		modelName = dbSettings.Model
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = dbSettings.MaxTokens
	}
	temperature := opts.Temperature
	if temperature == 0 {
		temperature = dbSettings.Temperature
	}
	// Runtime-switchable thinking budget (chat UI /thinking): the startup
	// flag wins when pinned, otherwise the persisted setting applies.
	thinkingBudget := opts.ThinkingBudget
	if thinkingBudget == 0 {
		thinkingBudget = dbSettings.ThinkingBudget
	}

	cfg := config.Config{
		Provider:          config.Provider(provider),
		Model:             modelName,
		APIKey:            ResolveKey(opts.APIKey, provider, st),
		BaseURL:           ResolveBaseURL(opts.BaseURL, provider, dbSettings),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		PermissionMode:    mode,
		WorkDir:           workdir,
		ExtraSystemPrompt: opts.ExtraPrompt,
		ThinkingBudget:    thinkingBudget,
	}
	return cfg.Normalize()
}
