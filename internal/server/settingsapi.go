package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/projanvil/woca/internal/msgplane"
	"github.com/projanvil/woca/internal/settings"
)

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// appSettings is the settings API payload. Secret fields are write-only:
// reads expose only whether they are set.
type appSettings struct {
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	MaxTokens          int     `json:"max_tokens"`
	Temperature        float64 `json:"temperature"`
	AnthropicBaseURL   string  `json:"anthropic_base_url"`
	OpenAIBaseURL      string  `json:"openai_base_url"`
	GLMBaseURL         string  `json:"glm_base_url"`
	DeepSeekBaseURL    string  `json:"deepseek_base_url"`
	AnthropicAPIKeySet bool    `json:"anthropic_api_key_set"`
	OpenAIAPIKeySet    bool    `json:"openai_api_key_set"`
	GLMAPIKeySet       bool    `json:"glm_api_key_set"`
	DeepSeekAPIKeySet  bool    `json:"deepseek_api_key_set"`
	CheckpointBackend  string  `json:"checkpoint_backend"`
	MemoryBackend      string  `json:"memory_backend"`
	CheckpointDir      string  `json:"checkpoint_dir"`
	PostgresURLSet     bool    `json:"postgres_url_set"`
	Database           string  `json:"database"`
	DebugLog           bool    `json:"debug_log"`
	LogDir             string  `json:"log_dir"`

	MemoryEngine            string `json:"memory_engine"`
	LadyMDBPath             string `json:"ladym_db_path"`
	LadyMEmbedding          string `json:"ladym_embedding"`
	LadyMEmbeddingModel     string `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL   string `json:"ladym_embedding_base_url"`
	LadyMEmbeddingAPIKeySet bool   `json:"ladym_embedding_api_key_set"`
	LadyMLLM                string `json:"ladym_llm"`
	LadyMAutoRetain         bool   `json:"ladym_autoretain"`
	LadyMTopK               int    `json:"ladym_topk"`
	LadyMSystem2            bool   `json:"ladym_system2"`
	LadyMSystem2Interval    int    `json:"ladym_system2_interval"`

	// Message plane (run dispatch): pg | pg+nats, live-reloaded.
	MsgPlaneMode    string `json:"msg_plane_mode"`
	MsgPlaneNATSURL string `json:"msg_plane_nats_url"`
	MsgPlaneSubject string `json:"msg_plane_subject"`
	// MsgPlaneAvailable reports whether this deployment dispatches runs at
	// all (the enterprise queue-dispatch form). Personal editions keep the
	// plane section hidden and its keys read-only no-ops.
	MsgPlaneAvailable bool `json:"msg_plane_available"`
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
	GLMBaseURL        *string  `json:"glm_base_url"`
	DeepSeekBaseURL   *string  `json:"deepseek_base_url"`
	AnthropicAPIKey   *string  `json:"anthropic_api_key"`
	OpenAIAPIKey      *string  `json:"openai_api_key"`
	GLMAPIKey         *string  `json:"glm_api_key"`
	DeepSeekAPIKey    *string  `json:"deepseek_api_key"`
	CheckpointBackend *string  `json:"checkpoint_backend"`
	MemoryBackend     *string  `json:"memory_backend"`
	CheckpointDir     *string  `json:"checkpoint_dir"`
	PostgresURL       *string  `json:"postgres_url"`
	DebugLog          *bool    `json:"debug_log"`

	MemoryEngine          *string `json:"memory_engine"`
	LadyMDBPath           *string `json:"ladym_db_path"`
	LadyMEmbedding        *string `json:"ladym_embedding"`
	LadyMEmbeddingModel   *string `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL *string `json:"ladym_embedding_base_url"`
	LadyMEmbeddingAPIKey  *string `json:"ladym_embedding_api_key"`
	LadyMLLM              *string `json:"ladym_llm"`
	LadyMAutoRetain       *bool   `json:"ladym_autoretain"`
	LadyMTopK             *int    `json:"ladym_topk"`
	LadyMSystem2          *bool   `json:"ladym_system2"`
	LadyMSystem2Interval  *int    `json:"ladym_system2_interval"`

	MsgPlaneMode    *string `json:"msg_plane_mode"`
	MsgPlaneNATSURL *string `json:"msg_plane_nats_url"`
	MsgPlaneSubject *string `json:"msg_plane_subject"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	st := s.settings
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	cfg := st.Load()
	logDir := ""
	if s.agent != nil && s.agent.Obs() != nil {
		logDir = s.agent.Obs().Root()
	}
	writeJSON(w, http.StatusOK, appSettings{
		Provider:           cfg.Provider,
		Model:              cfg.Model,
		MaxTokens:          cfg.MaxTokens,
		Temperature:        cfg.Temperature,
		AnthropicBaseURL:   cfg.AnthropicBaseURL,
		OpenAIBaseURL:      cfg.OpenAIBaseURL,
		GLMBaseURL:         cfg.GLMBaseURL,
		DeepSeekBaseURL:    cfg.DeepSeekBaseURL,
		AnthropicAPIKeySet: st.HasSecret(settings.SecretAnthropicAPIKey),
		OpenAIAPIKeySet:    st.HasSecret(settings.SecretOpenAIAPIKey),
		GLMAPIKeySet:       st.HasSecret(settings.SecretGLMAPIKey),
		DeepSeekAPIKeySet:  st.HasSecret(settings.SecretDeepSeekAPIKey),
		CheckpointBackend:  cfg.CheckpointBackend,
		MemoryBackend:      cfg.MemoryBackend,
		CheckpointDir:      cfg.CheckpointDir,
		PostgresURLSet:     st.HasSecret(settings.SecretPostgresURL),
		Database:           st.Path(),
		DebugLog:           cfg.DebugLog,
		LogDir:             logDir,

		MemoryEngine:            cfg.MemoryEngine,
		LadyMDBPath:             cfg.LadyMDBPath,
		LadyMEmbedding:          cfg.LadyMEmbedding,
		LadyMEmbeddingModel:     cfg.LadyMEmbeddingModel,
		LadyMEmbeddingBaseURL:   cfg.LadyMEmbeddingBaseURL,
		LadyMEmbeddingAPIKeySet: st.HasSecret(settings.SecretLadyMEmbeddingAPIKey),
		LadyMLLM:                cfg.LadyMLLM,
		LadyMAutoRetain:         cfg.LadyMAutoRetain,
		LadyMTopK:               cfg.LadyMTopK,
		LadyMSystem2:            cfg.LadyMSystem2,
		LadyMSystem2Interval:    cfg.LadyMSystem2Interval,
		MsgPlaneMode:            orDefaultStr(cfg.MsgPlaneMode, "pg"),
		MsgPlaneNATSURL:         cfg.MsgPlaneNATSURL,
		MsgPlaneSubject:         orDefaultStr(cfg.MsgPlaneSubject, msgplane.DefaultSubject),
	})
}

// Settings-update side effects, combined as bit flags per field.
const (
	effModel    uint8 = 1 << iota // rebuild the chat model
	effMemory                     // rebuild the memory engine
	effRestart                    // takes effect after a server restart
	effDebugLog                   // flip the observability store live
)

// fieldPatch is one updatable settings field: the settings key, whether the
// request touched it, the encoded value for settings.Apply, and the side
// effects applying it triggers. The table replaces one handwritten
// if-Apply-flag block per field.
type fieldPatch struct {
	key     string
	set     bool
	value   string
	effects uint8
}

func strPatch(key string, p *string, effects uint8) fieldPatch {
	if p == nil {
		return fieldPatch{key: key, effects: effects}
	}
	return fieldPatch{key: key, set: true, value: *p, effects: effects}
}

func intPatch(key string, p *int, effects uint8) fieldPatch {
	if p == nil {
		return fieldPatch{key: key, effects: effects}
	}
	return fieldPatch{key: key, set: true, value: strconv.Itoa(*p), effects: effects}
}

func floatPatch(key string, p *float64, effects uint8) fieldPatch {
	if p == nil {
		return fieldPatch{key: key, effects: effects}
	}
	return fieldPatch{key: key, set: true, value: ftoa(*p), effects: effects}
}

func boolPatch(key string, p *bool, effects uint8) fieldPatch {
	if p == nil {
		return fieldPatch{key: key, effects: effects}
	}
	return fieldPatch{key: key, set: true, value: strconv.FormatBool(*p), effects: effects}
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

	patches := []fieldPatch{
		// Model keys rebuild the live chat model.
		strPatch(settings.KeyProvider, req.Provider, effModel),
		strPatch(settings.KeyModel, req.Model, effModel),
		intPatch(settings.KeyMaxTokens, req.MaxTokens, effModel),
		floatPatch(settings.KeyTemperature, req.Temperature, effModel),
		strPatch(settings.KeyAnthropicBaseURL, req.AnthropicBaseURL, effModel),
		strPatch(settings.KeyOpenAIBaseURL, req.OpenAIBaseURL, effModel),
		strPatch(settings.KeyGLMBaseURL, req.GLMBaseURL, effModel),
		strPatch(settings.KeyDeepSeekBaseURL, req.DeepSeekBaseURL, effModel),
		// Storage backends only take effect after a restart.
		strPatch(settings.KeyCheckpoint, req.CheckpointBackend, effRestart),
		strPatch(settings.KeyMemory, req.MemoryBackend, effRestart),
		strPatch(settings.KeyCheckpointDir, req.CheckpointDir, effRestart),
		// Memory-engine keys participate in engine construction: they
		// trigger a live rebuild (no restart required).
		strPatch(settings.KeyMemoryEngine, req.MemoryEngine, effMemory),
		strPatch(settings.KeyLadyMDBPath, req.LadyMDBPath, effMemory),
		strPatch(settings.KeyLadyMEmbedding, req.LadyMEmbedding, effMemory),
		strPatch(settings.KeyLadyMEmbeddingModel, req.LadyMEmbeddingModel, effMemory),
		strPatch(settings.KeyLadyMEmbeddingBaseURL, req.LadyMEmbeddingBaseURL, effMemory),
		strPatch(settings.KeyLadyMLLM, req.LadyMLLM, effMemory),
		// Per-run keys are re-read on each turn: no rebuild, no restart.
		boolPatch(settings.KeyLadyMAutoRetain, req.LadyMAutoRetain, 0),
		intPatch(settings.KeyLadyMTopK, req.LadyMTopK, 0),
		boolPatch(settings.KeyLadyMSystem2, req.LadyMSystem2, 0),
		intPatch(settings.KeyLadyMSystem2Interval, req.LadyMSystem2Interval, 0),
		boolPatch(settings.KeyDebugLog, req.DebugLog, effDebugLog),
	}

	// The message plane is an enterprise feature (design §5.4): it only
	// exists where runs are dispatched to workers. Requests from a
	// personal edition (no queue dispatch) are rejected rather than
	// silently storing keys nothing will read.
	if !s.dispatch {
		for _, f := range []struct {
			set bool
			k   string
		}{
			{req.MsgPlaneMode != nil, settings.KeyMsgPlaneMode},
			{req.MsgPlaneNATSURL != nil, settings.KeyMsgPlaneNATSURL},
			{req.MsgPlaneSubject != nil, settings.KeyMsgPlaneSubject},
		} {
			if f.set {
				writeError(w, http.StatusBadRequest, f.k+" is an enterprise message-plane setting; this deployment does not dispatch runs")
				return
			}
		}
	} else {
		// Message-plane keys are resolved per dispatch/claim by the plane
		// (internal/msgplane), so changes apply live — no rebuild, no
		// restart, and the PG queue keeps executing throughout.
		patches = append(patches,
			strPatch(settings.KeyMsgPlaneMode, req.MsgPlaneMode, 0),
			strPatch(settings.KeyMsgPlaneNATSURL, req.MsgPlaneNATSURL, 0),
			strPatch(settings.KeyMsgPlaneSubject, req.MsgPlaneSubject, 0),
		)
	}

	cfg := st.Load()
	var applied uint8
	for _, p := range patches {
		if !p.set {
			continue
		}
		if err := settings.Apply(&cfg, p.key, p.value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		applied |= p.effects
	}
	if err := st.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Secrets: non-empty value stores (encrypted); present-but-empty clears.
	secrets := []fieldPatch{
		strPatch(settings.SecretAnthropicAPIKey, req.AnthropicAPIKey, effModel),
		strPatch(settings.SecretOpenAIAPIKey, req.OpenAIAPIKey, effModel),
		strPatch(settings.SecretGLMAPIKey, req.GLMAPIKey, effModel),
		strPatch(settings.SecretDeepSeekAPIKey, req.DeepSeekAPIKey, effModel),
		strPatch(settings.SecretPostgresURL, req.PostgresURL, effRestart),
		strPatch(settings.SecretLadyMEmbeddingAPIKey, req.LadyMEmbeddingAPIKey, effMemory),
	}
	for _, sec := range secrets {
		if !sec.set {
			continue
		}
		if err := st.SetSecret(sec.key, sec.value); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		applied |= sec.effects
	}

	applyErr := ""
	if applied&effModel != 0 && s.rebuildModel != nil {
		if err := s.rebuildModel(); err != nil {
			applyErr = err.Error()
		}
	}
	if applied&effMemory != 0 && s.rebuildMemory != nil {
		if err := s.rebuildMemory(); err != nil {
			if applyErr != "" {
				applyErr += "; "
			}
			applyErr += err.Error()
		}
	}
	// The observability log toggles live (no restart): flip the store right
	// after the settings row is saved.
	if applied&effDebugLog != 0 && s.agent != nil && s.agent.Obs() != nil {
		s.agent.Obs().SetEnabled(cfg.DebugLog)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": applied&effRestart != 0,
		"apply_error":      applyErr,
	})
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
