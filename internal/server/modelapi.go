package server

import (
	"net/http"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// handleListModels serves GET /api/models: the operator-authored model
// presets (config plane, design §16), without credentials. The consumer
// frontend offers them for selection or lets the user type freely.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	var presets []wocadb.ModelPreset
	var err error
	switch {
	case s.modelCatalog != nil:
		// The authoritative catalog lives behind an injected fetcher.
		presets, err = s.modelCatalog(r.Context())
	case s.settings != nil && s.settings.DB() != nil:
		presets, err = s.settings.DB().ListModelPresets()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		out = append(out, map[string]any{
			"id":       p.ID,
			"name":     p.Name,
			"provider": p.Provider,
			"model":    p.Model,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// providerKeySecret maps a provider to its settings secret slot.
func providerKeySecret(provider string) string {
	switch provider {
	case "openai":
		return settings.SecretOpenAIAPIKey
	case "glm":
		return settings.SecretGLMAPIKey
	case "deepseek":
		return settings.SecretDeepSeekAPIKey
	default:
		return settings.SecretAnthropicAPIKey
	}
}

// providerBaseURLKey maps a provider to its base-url settings key.
func providerBaseURLKey(provider string) (string, bool) {
	switch provider {
	case "anthropic":
		return settings.KeyAnthropicBaseURL, true
	case "openai":
		return settings.KeyOpenAIBaseURL, true
	case "glm":
		return settings.KeyGLMBaseURL, true
	case "deepseek":
		return settings.KeyDeepSeekBaseURL, true
	}
	return "", false
}

// handleApplyModel serves POST /api/models/{id}/apply: copy the preset
// into the live settings (provider, model, limits, base URL, and the
// preset's stored key into the provider's secret slot) and rebuild the
// chat model. The key stays server-side throughout.
func (s *Server) handleApplyModel(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil || s.settings.DB() == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	id := r.PathValue("id")
	var preset *wocadb.ModelPreset
	presetKey := ""
	if s.resolveModel != nil {
		// The authoritative catalog decides: unknown models are refused
		// before anything local is touched.
		p, key, ok := s.resolveModel(r.Context(), id, "", "")
		if !ok {
			http.NotFound(w, r)
			return
		}
		preset, presetKey = &p, key
	} else {
		if s.settings == nil || s.settings.DB() == nil {
			writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
			return
		}
		presets, err := s.settings.DB().ListModelPresets()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i := range presets {
			if presets[i].ID == id {
				preset = &presets[i]
				break
			}
		}
		if preset == nil {
			http.NotFound(w, r)
			return
		}
		presetKey = s.settings.GetSecret(wocadb.ModelPresetKeySecret(preset.ID))
	}

	cfg := s.settings.Load()
	cfg.Provider = preset.Provider
	cfg.Model = preset.Model
	if preset.MaxTokens > 0 {
		cfg.MaxTokens = int(preset.MaxTokens)
	}
	if preset.Temperature > 0 {
		cfg.Temperature = preset.Temperature
	}
	if key, ok := providerBaseURLKey(preset.Provider); ok {
		// Empty base URL clears the override — the provider's official
		// endpoint — matching the console's field semantics.
		_ = settings.Apply(&cfg, key, preset.BaseURL)
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The preset's key, when authorized, becomes the provider's live key.
	if key := presetKey; key != "" {
		if err := s.settings.SetSecret(providerKeySecret(preset.Provider), key); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	applyErr := ""
	if s.rebuildModel != nil {
		if err := s.rebuildModel(); err != nil {
			applyErr = err.Error()
		}
	}
	s.log.Info().Str("preset", preset.ID).Str("model", preset.Model).Msg("model preset applied")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"provider":    preset.Provider,
		"model":       preset.Model,
		"apply_error": applyErr,
	})
}
