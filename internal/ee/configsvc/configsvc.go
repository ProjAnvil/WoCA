// Package configsvc is the standalone settings management plane (design
// §16): a dedicated role that owns configuration read/write, validation,
// export/import, and its console SPA. Business roles (woca-server,
// woca-worker) keep reading the shared settings tables directly — the
// config service is the sole management entry, deployed independently
// behind the gateway's admin port.
//
// Product design v1.8: this console is focused on woca's own settings
// domains (models, storage, memory, dispatch). Tenant and bridge
// governance — credentials, sessions, enrollments, the registration
// inventory, the governance audit — lives on the service plane (the
// tenant ops console and the core identity service). The config package
// carries NO user-identity material: bridges hold zero user credentials
// and validate against the core online.
package configsvc

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/projanvil/woca/internal/platform/httpx"

	"github.com/projanvil/woca/internal/ee/consoleauth"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/msgplane"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

//go:embed all:spa
var spaFS embed.FS

// Service wires the config plane over a settings store and a tenant store.
type Service struct {
	settings *settings.Store
	tenants  *tenant.Store
	db       *wocadb.DB
	plane    *msgplane.Plane
	log      zerolog.Logger
	entrygw  *entrygw.ConfigHandler
	console  *consoleauth.Gate

	// wakeMu/wakeCh implement the config-write broadcast: every notify
	// closes the current channel and installs a fresh one (close-to-
	// broadcast); the entry gateway's blocking query selects on it.
	wakeMu sync.Mutex
	wakeCh chan struct{}
}

// Wake returns the current config-write broadcast channel (close-and-
// replace semantics: a signal wakes every waiter exactly once).
func (s *Service) Wake() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeCh == nil {
		s.wakeCh = make(chan struct{})
	}
	return s.wakeCh
}

// broadcast closes the pending wake channel so blocking config queries
// re-check the generation immediately.
func (s *Service) broadcast() {
	s.wakeMu.Lock()
	ch := s.wakeCh
	s.wakeCh = make(chan struct{})
	s.wakeMu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// New builds the service (both stores over the same shared database). db
// backs the config-ack inventory; plane, when non-nil, pushes change
// notifications over the message plane (design §16.7).
func New(st *settings.Store, ts *tenant.Store, db *wocadb.DB, plane *msgplane.Plane, log zerolog.Logger) (*Service, error) {
	// Timestamp columns hold unix milliseconds — BIGINT on postgres
	// (int4 overflows; the house convention).
	tsCol := "INTEGER"
	if db.Dialect == wocadb.DialectPostgres {
		tsCol = "BIGINT"
	}
	if err := db.WithMigrateLock(func(q wocadb.Querier) error {
		ddls := []string{
			`CREATE TABLE IF NOT EXISTS managed_skills (
				name TEXT PRIMARY KEY,
				description TEXT NOT NULL DEFAULT '',
				body TEXT NOT NULL,
				updated_at ` + tsCol + ` NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS managed_mcp (
				name TEXT PRIMARY KEY,
				spec TEXT NOT NULL,
				updated_at ` + tsCol + ` NOT NULL
			)`,
		}
		if db.Dialect == wocadb.DialectPostgres {
			// Widen pre-existing int4 columns (the migration landed after
			// the first boot had already created them).
			ddls = append(ddls,
				`ALTER TABLE managed_skills ALTER COLUMN updated_at TYPE BIGINT`,
				`ALTER TABLE managed_mcp ALTER COLUMN updated_at TYPE BIGINT`)
		}
		for _, ddl := range ddls {
			if _, err := q.Exec(ddl); err != nil {
				return fmt.Errorf("configsvc: skills/mcp migrate: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	gwStore, err := entrygw.NewStore(db)
	if err != nil {
		return nil, fmt.Errorf("configsvc: entry gateway: %w", err)
	}
	svc := &Service{settings: st, tenants: ts, db: db, plane: plane, log: log}
	svc.entrygw = entrygw.NewConfigHandler(gwStore, svc.bridgeConfig)
	svc.entrygw.Wake = svc.Wake
	svc.console = consoleauth.New()
	return svc, nil
}

// notifyConfig publishes the new generation after every write so
// subscribed roles rebuild immediately instead of waiting for the poll.
func (s *Service) notifyConfig() {
	s.broadcast()
	if s.plane != nil {
		s.plane.NotifyConfig(s.settings.Generation())
	}
}

// Handler returns the full HTTP handler: the console gate (open or
// session-cookie login), per-route scope checks, and the console SPA on
// non-API paths.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-config"})
	})
	mux.Handle("GET /api/settings", s.requireScope(authz.ScopeSettingsRead, s.handleGet))
	mux.Handle("PUT /api/settings", s.requireScope(authz.ScopeSettingsWrite, s.handleUpdate))
	mux.Handle("GET /api/settings/export", s.requireScope(authz.ScopeSettingsRead, s.handleExport))
	mux.Handle("POST /api/settings/import", s.requireScope(authz.ScopeSettingsWrite, s.handleImport))
	mux.Handle("GET /api/config/status", s.requireScope(authz.ScopeSettingsRead, s.handleStatus))
	// Model presets (design §16): named model configurations authored by
	// operators. No active flag — consumers pick.
	mux.Handle("GET /api/models", s.requireScope(authz.ScopeSettingsRead, s.handleListModels))
	mux.Handle("POST /api/models", s.requireScope(authz.ScopeSettingsWrite, s.handleSaveModel))
	mux.Handle("DELETE /api/models/{id}", s.requireScope(authz.ScopeSettingsWrite, s.handleDeleteModel))
	// Operator-delivered skills & MCP (§16): the admin authors them here;
	// enrolled bridges materialize them as their lowest merge layer.
	mux.Handle("GET /api/skills", s.requireScope(authz.ScopeSettingsRead, s.handleListManagedSkills))
	mux.Handle("PUT /api/skills", s.requireScope(authz.ScopeSettingsWrite, s.handlePutManagedSkill))
	mux.Handle("DELETE /api/skills/{name}", s.requireScope(authz.ScopeSettingsWrite, s.handleDeleteManagedSkill))
	mux.Handle("GET /api/mcp", s.requireScope(authz.ScopeSettingsRead, s.handleListManagedMCP))
	mux.Handle("PUT /api/mcp", s.requireScope(authz.ScopeSettingsWrite, s.handlePutManagedMCP))
	mux.Handle("DELETE /api/mcp/{name}", s.requireScope(authz.ScopeSettingsWrite, s.handleDeleteManagedMCP))
	spaRoot, err := fs.Sub(spaFS, "spa")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", spaWithFallback(spaRoot))

	gwMux := http.NewServeMux()
	// The config plane keeps the /gw/config pull (v1.5 拓扑拆分) plus the
	// model catalog bridges pull on demand (§16: presets are config-plane
	// assets — bridges fetch the list and verify their preferred model
	// here; enrollment/renewal/heartbeat live on the service plane).
	s.entrygw.RegisterRoutes(gwMux)
	gwMux.Handle("GET /gw/models", s.entrygw.RouteAuth(s.handleGWListModels))
	gwMux.Handle("POST /gw/models/verify", s.entrygw.RouteAuth(s.handleGWVerifyModel))
	top := http.NewServeMux()
	top.Handle("/gw/", gwMux)
	// Console credential exchange: mounted at the top level — the login
	// route must be reachable before a session exists, so it cannot live
	// behind the gate it bootstraps.
	top.HandleFunc("POST /api/console/login", s.console.Login)
	top.HandleFunc("POST /api/console/logout", s.console.Logout)
	// The console surface: open when no credentials are configured,
	// session-gated otherwise. The machine channel and the login route
	// above never pass through this gate.
	top.Handle("/", s.console.Wrap(mux))
	return top
}

// patches translates an update into (key, value) pairs for settings.Apply.
// Message-plane keys ride along: the config plane does not know which
// deployment forms dispatch runs, so it stores them; readers that lack the
// dispatch form ignore them (the business plane additionally rejects them
// at its own API).
func (u *update) patches() []patch {
	p := []patch{
		{settings.KeyProvider, u.Provider},
		{settings.KeyModel, u.Model},
		{settings.KeyMaxTokens, intPtr(u.MaxTokens)},
		{settings.KeyTemperature, floatPtr(u.Temperature)},
		{settings.KeyAnthropicBaseURL, u.AnthropicBaseURL},
		{settings.KeyOpenAIBaseURL, u.OpenAIBaseURL},
		{settings.KeyGLMBaseURL, u.GLMBaseURL},
		{settings.KeyDeepSeekBaseURL, u.DeepSeekBaseURL},
		{settings.KeyCheckpoint, u.CheckpointBackend},
		{settings.KeyMemory, u.MemoryBackend},
		{settings.KeyCheckpointDir, u.CheckpointDir},
		{settings.KeyMemoryEngine, u.MemoryEngine},
		{settings.KeyLadyMDBPath, u.LadyMDBPath},
		{settings.KeyLadyMEmbedding, u.LadyMEmbedding},
		{settings.KeyLadyMEmbeddingModel, u.LadyMEmbeddingModel},
		{settings.KeyLadyMEmbeddingBaseURL, u.LadyMEmbeddingBaseURL},
		{settings.KeyLadyMLLM, u.LadyMLLM},
		{settings.KeyLadyMAutoRetain, u.boolPtr(u.LadyMAutoRetain)},
		{settings.KeyLadyMTopK, intPtr(u.LadyMTopK)},
		{settings.KeyLadyMSystem2, u.boolPtr(u.LadyMSystem2)},
		{settings.KeyLadyMSystem2Interval, intPtr(u.LadyMSystem2Interval)},
		{settings.KeyDebugLog, u.boolPtr(u.DebugLog)},
		{settings.KeyLadyMEndpoint, u.LadyMEndpoint},
		{settings.KeyLadyMAPIUsername, u.LadyMAPIUsername},
		{settings.KeyCheckpointPGHost, u.CheckpointPGHost},
		{settings.KeyCheckpointPGPort, u.CheckpointPGPort},
		{settings.KeyCheckpointPGDB, u.CheckpointPGDB},
		{settings.KeyCheckpointPGUser, u.CheckpointPGUser},
		{settings.KeyMemoryPGHost, u.MemoryPGHost},
		{settings.KeyMemoryPGPort, u.MemoryPGPort},
		{settings.KeyMemoryPGDB, u.MemoryPGDB},
		{settings.KeyMemoryPGUser, u.MemoryPGUser},
		{settings.KeyMsgPlaneMode, u.MsgPlaneMode},
		{settings.KeyMsgPlaneNATSURL, u.MsgPlaneNATSURL},
		{settings.KeyMsgPlaneSubject, u.MsgPlaneSubject},
	}
	// The enterprise plugin set (a JSON array of enabled built-ins) rides
	// as one settings key; present means "pin this set", absent leaves
	// plugins to the bridge's users.
	if u.BuiltinPlugins != nil {
		if raw, err := json.Marshal(*u.BuiltinPlugins); err == nil {
			p = append(p, patch{key: settings.KeyBuiltinPlugins, value: ptr(string(raw))})
		}
	}
	out := make([]patch, 0, len(p))
	for _, e := range p {
		if e.value != nil {
			out = append(out, e)
		}
	}
	return out
}

// secrets lists the update's secret writes (non-empty stores, empty clears).
func (u *update) secretPatches() []patch {
	return []patch{
		{settings.SecretCheckpointPGPassword, u.CheckpointPGPassword},
		{settings.SecretMemoryPGPassword, u.MemoryPGPassword},
		{settings.SecretPostgresURL, u.PostgresURL},
		{settings.SecretLadyMAPIPassword, u.LadyMAPIPassword},
		{settings.SecretAnthropicAPIKey, u.AnthropicAPIKey},
		{settings.SecretOpenAIAPIKey, u.OpenAIAPIKey},
		{settings.SecretGLMAPIKey, u.GLMAPIKey},
		{settings.SecretDeepSeekAPIKey, u.DeepSeekAPIKey},
		{settings.SecretLadyMEmbeddingAPIKey, u.LadyMEmbeddingAPIKey},
	}
}

var modelProviders = map[string]bool{"anthropic": true, "openai": true, "glm": true, "deepseek": true}

func (s *Service) handleListModels(w http.ResponseWriter, _ *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusOK, []modelPayload{})
		return
	}
	presets, err := s.db.ListModelPresets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]modelPayload, 0, len(presets))
	for _, p := range presets {
		out = append(out, modelPayload{
			ID: p.ID, Name: p.Name, Provider: p.Provider, Model: p.Model,
			MaxTokens: p.MaxTokens, Temperature: p.Temperature, BaseURL: p.BaseURL,
			KeySet:    s.settings.HasSecret(wocadb.ModelPresetKeySecret(p.ID)),
			UpdatedAt: p.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSaveModel upserts one preset. An empty id mints one; the API key
// (when non-empty) lands in the encrypted secrets table.
func (s *Service) handleSaveModel(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "preset storage unavailable")
		return
	}
	var req struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Provider    string  `json:"provider"`
		Model       string  `json:"model"`
		MaxTokens   int64   `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		BaseURL     string  `json:"base_url"`
		APIKey      string  `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "name and model are required")
		return
	}
	if !modelProviders[req.Provider] {
		writeError(w, http.StatusBadRequest, "provider must be anthropic, openai, glm, or deepseek")
		return
	}
	now := wocadb.Now()
	p := wocadb.ModelPreset{
		ID: req.ID, Name: req.Name, Provider: req.Provider, Model: req.Model,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature, BaseURL: req.BaseURL,
		UpdatedAt: now,
	}
	if p.ID == "" {
		p.ID = fmt.Sprintf("mp-%x", now)
		p.CreatedAt = now
	} else if existing, err := s.db.ListModelPresets(); err == nil {
		for _, e := range existing {
			if e.ID == p.ID {
				p.CreatedAt = e.CreatedAt
				break
			}
		}
	}
	if err := s.db.SaveModelPreset(p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.APIKey != "" {
		if err := s.settings.SetSecret(wocadb.ModelPresetKeySecret(p.ID), req.APIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.log.Info().Str("preset", p.ID).Str("name", p.Name).Msg("model preset saved via config plane")
	s.notifyConfig()
	writeJSON(w, http.StatusOK, modelPayload{
		ID: p.ID, Name: p.Name, Provider: p.Provider, Model: p.Model,
		MaxTokens: p.MaxTokens, Temperature: p.Temperature, BaseURL: p.BaseURL,
		KeySet:    s.settings.HasSecret(wocadb.ModelPresetKeySecret(p.ID)),
		UpdatedAt: p.UpdatedAt,
	})
}

func (s *Service) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "preset storage unavailable")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "preset id is required")
		return
	}
	if err := s.db.DeleteModelPreset(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info().Str("preset", id).Msg("model preset deleted via config plane")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func updateKeys(u *update) []string {
	var keys []string
	for _, p := range u.patches() {
		keys = append(keys, p.key)
	}
	for _, p := range u.secretPatches() {
		if p.value != nil {
			keys = append(keys, p.key)
		}
	}
	return keys
}

// spaWithFallback serves SPA assets and falls back to index.html for
// client-side routes (unknown non-API paths render the app).
func spaWithFallback(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	ix, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if _, err := fs.Stat(root, name); err != nil {
				// Client-side route: render the app shell.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(ix)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteError(w, status, msg)
}

func formatFloat(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
