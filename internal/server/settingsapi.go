package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/projanvil/woca/internal/settings"
)

// appSettings is the settings API payload. Secret fields are write-only:
// reads expose only whether they are set.
type appSettings struct {
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	MaxTokens          int     `json:"max_tokens"`
	Temperature        float64 `json:"temperature"`
	AnthropicBaseURL   string  `json:"anthropic_base_url"`
	OpenAIBaseURL      string  `json:"openai_base_url"`
	AnthropicAPIKeySet bool    `json:"anthropic_api_key_set"`
	OpenAIAPIKeySet    bool    `json:"openai_api_key_set"`
	CheckpointBackend  string  `json:"checkpoint_backend"`
	MemoryBackend      string  `json:"memory_backend"`
	CheckpointDir      string  `json:"checkpoint_dir"`
	PostgresURLSet     bool    `json:"postgres_url_set"`
	Database           string  `json:"database"`
}

// appSettingsUpdate is the PUT body. Pointer fields distinguish "unchanged"
// (absent) from "clear" (present but empty).
type appSettingsUpdate struct {
	Provider          *string  `json:"provider"`
	Model             *string  `json:"model"`
	MaxTokens         *int     `json:"max_tokens"`
	Temperature       *float64 `json:"temperature"`
	AnthropicBaseURL  *string  `json:"anthropic_base_url"`
	OpenAIBaseURL     *string  `json:"openai_base_url"`
	AnthropicAPIKey   *string  `json:"anthropic_api_key"`
	OpenAIAPIKey      *string  `json:"openai_api_key"`
	CheckpointBackend *string  `json:"checkpoint_backend"`
	MemoryBackend     *string  `json:"memory_backend"`
	CheckpointDir     *string  `json:"checkpoint_dir"`
	PostgresURL       *string  `json:"postgres_url"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	st := s.settings
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	cfg := st.Load()
	writeJSON(w, http.StatusOK, appSettings{
		Provider:           cfg.Provider,
		Model:              cfg.Model,
		MaxTokens:          cfg.MaxTokens,
		Temperature:        cfg.Temperature,
		AnthropicBaseURL:   cfg.AnthropicBaseURL,
		OpenAIBaseURL:      cfg.OpenAIBaseURL,
		AnthropicAPIKeySet: st.HasSecret(settings.SecretAnthropicAPIKey),
		OpenAIAPIKeySet:    st.HasSecret(settings.SecretOpenAIAPIKey),
		CheckpointBackend:  cfg.CheckpointBackend,
		MemoryBackend:      cfg.MemoryBackend,
		CheckpointDir:      cfg.CheckpointDir,
		PostgresURLSet:     st.HasSecret(settings.SecretPostgresURL),
		Database:           st.Path(),
	})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	st := s.settings
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	var req appSettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := st.Load()
	restartRequired := false
	modelChanged := false

	if req.Provider != nil {
		if err := settings.Apply(&cfg, settings.KeyProvider, *req.Provider); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.Model != nil {
		if err := settings.Apply(&cfg, settings.KeyModel, *req.Model); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.MaxTokens != nil {
		if err := settings.Apply(&cfg, settings.KeyMaxTokens, itoa(*req.MaxTokens)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.Temperature != nil {
		if err := settings.Apply(&cfg, settings.KeyTemperature, ftoa(*req.Temperature)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.AnthropicBaseURL != nil {
		if err := settings.Apply(&cfg, settings.KeyAnthropicBaseURL, *req.AnthropicBaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.OpenAIBaseURL != nil {
		if err := settings.Apply(&cfg, settings.KeyOpenAIBaseURL, *req.OpenAIBaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelChanged = true
	}
	if req.CheckpointBackend != nil {
		if err := settings.Apply(&cfg, settings.KeyCheckpoint, *req.CheckpointBackend); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		restartRequired = true
	}
	if req.MemoryBackend != nil {
		if err := settings.Apply(&cfg, settings.KeyMemory, *req.MemoryBackend); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		restartRequired = true
	}
	if req.CheckpointDir != nil {
		if err := settings.Apply(&cfg, settings.KeyCheckpointDir, *req.CheckpointDir); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		restartRequired = true
	}
	if err := st.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Secrets: non-empty value stores (encrypted); present-but-empty clears.
	if req.AnthropicAPIKey != nil {
		if err := st.SetSecret(settings.SecretAnthropicAPIKey, *req.AnthropicAPIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		modelChanged = true
	}
	if req.OpenAIAPIKey != nil {
		if err := st.SetSecret(settings.SecretOpenAIAPIKey, *req.OpenAIAPIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		modelChanged = true
	}
	if req.PostgresURL != nil {
		if err := st.SetSecret(settings.SecretPostgresURL, *req.PostgresURL); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		restartRequired = true
	}

	applyErr := ""
	if modelChanged && s.rebuildModel != nil {
		if err := s.rebuildModel(); err != nil {
			applyErr = err.Error()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
		"apply_error":      applyErr,
	})
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
