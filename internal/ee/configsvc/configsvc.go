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
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/projanvil/woca/internal/platform/httpx"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/projanvil/woca/internal/ee/consoleauth"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/msgplane"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/gateway"
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
	svc.console = consoleauth.New()
	return svc, nil
}

// bridgeConfig is the config-plane payload for one bridge (product design
// §3): the managed-domain settings with the generation fingerprint as the
// version. Provider API keys travel this authenticated bridge↔gateway
// channel only — they never reach clients (D5/R4).
func (s *Service) bridgeConfig(_ context.Context, _ string) (string, map[string]string, error) {
	cfg := s.settings.Load()
	// provider/model deliberately do NOT ride the package (product
	// ruling: the model choice is a bridge-side user action governed by
	// the catalog pull + verify — the core's global provider/model
	// values would lock the bridge's selection). The catalog IS the
	// control surface; base URLs and secrets remain managed.
	kv := map[string]string{
		"anthropic-base-url": cfg.AnthropicBaseURL,
		"openai-base-url":    cfg.OpenAIBaseURL,
		"glm-base-url":       cfg.GLMBaseURL,
		"deepseek-base-url":  cfg.DeepSeekBaseURL,
		"checkpoint-backend": cfg.CheckpointBackend,
		"memory-backend":     cfg.MemoryBackend,
		"memory-engine":      cfg.MemoryEngine,
		"model-whitelist":    cfg.ModelWhitelist,
	}
	if cfg.MaxTokens != 0 {
		kv["max-tokens"] = fmt.Sprintf("%d", cfg.MaxTokens)
	}
	// Storage/memory wiring rides along whenever the split postgres
	// coordinates or the remote ladyM endpoint are configured: without
	// them a backend/engine delivery leaves the bridge nothing to connect
	// to (§3 storage domain — bridges use the organization's
	// infrastructure, never a local store they picked themselves).
	if cfg.CheckpointPGHost != "" {
		kv["checkpoint-pg-host"] = cfg.CheckpointPGHost
		kv["checkpoint-pg-port"] = cfg.CheckpointPGPort
		kv["checkpoint-pg-db"] = cfg.CheckpointPGDB
		kv["checkpoint-pg-user"] = cfg.CheckpointPGUser
	}
	if cfg.MemoryPGHost != "" {
		kv["memory-pg-host"] = cfg.MemoryPGHost
		kv["memory-pg-port"] = cfg.MemoryPGPort
		kv["memory-pg-db"] = cfg.MemoryPGDB
		kv["memory-pg-user"] = cfg.MemoryPGUser
	}
	if cfg.LadyMEndpoint != "" {
		kv["ladym-endpoint"] = cfg.LadyMEndpoint
		kv["ladym-api-username"] = cfg.LadyMAPIUsername
	}
	// The enterprise plugin set (a JSON array of enabled built-ins) only
	// rides when the operator pinned one; absent leaves plugins to the
	// bridge's users.
	if len(cfg.BuiltinPlugins) > 0 {
		if raw, err := json.Marshal(cfg.BuiltinPlugins); err == nil {
			kv["builtin-plugins"] = string(raw)
		}
	}
	for key, secret := range settings.ManagedSecretKeys {
		if v := s.settings.GetSecret(secret); v != "" {
			kv[key] = v
		}
	}
	// Operator-delivered skills & MCP (§16): they ride the package and
	// materialize as the bridge's lowest merge layer; user config still
	// wins.
	if skillsList := s.managedSkillsPayload(); len(skillsList) > 0 {
		if raw, err := json.Marshal(skillsList); err == nil {
			kv["managed-skills"] = string(raw)
		}
	}
	if servers := s.managedMCPPayload(); len(servers) > 0 {
		specs := make([]string, 0, len(servers))
		for _, m := range servers {
			specs = append(specs, m.Spec)
		}
		if raw, err := json.Marshal(specs); err == nil {
			kv["managed-mcp"] = string(raw)
		}
	}
	return s.settings.Generation(), kv, nil
}

// ManagedSkill is one operator-authored skill (SKILL.md body).
type ManagedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	UpdatedAt   int64  `json:"updated_at"`
}

// ManagedMCP is one operator-registered MCP server spec (name=command or
// name=http(s)://endpoint — the spec grammar the layered merger
// consumes).
type ManagedMCP struct {
	Name      string `json:"name"`
	Spec      string `json:"spec"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Service) managedSkillsPayload() []ManagedSkill {
	rows, err := s.db.Query(`SELECT name, description, body, updated_at FROM managed_skills ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ManagedSkill
	for rows.Next() {
		var sk ManagedSkill
		if err := rows.Scan(&sk.Name, &sk.Description, &sk.Body, &sk.UpdatedAt); err == nil {
			out = append(out, sk)
		}
	}
	return out
}

func (s *Service) managedMCPPayload() []ManagedMCP {
	rows, err := s.db.Query(`SELECT name, spec, updated_at FROM managed_mcp ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ManagedMCP
	for rows.Next() {
		var m ManagedMCP
		if err := rows.Scan(&m.Name, &m.Spec, &m.UpdatedAt); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// handleGWListModels serves the preset catalog to enrolled bridges —
// the fields the selection surface needs, never credentials: a preset's
// API key travels only inside the verify authorization (below).
func (s *Service) handleGWListModels(w http.ResponseWriter, _ *http.Request) {
	presets, err := s.db.ListModelPresets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	out := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "provider": p.Provider, "model": p.Model,
			"max_tokens": p.MaxTokens, "temperature": p.Temperature, "base_url": p.BaseURL,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGWVerifyModel is the run-time authorization (product ruling):
// the bridge presents the preferred model (preset id, or the
// provider+model pair); the config plane decides whether that model
// exists in the operator-authored catalog. Verified models return the
// full preset INCLUDING its API key — credential material that rides
// only this machine-authenticated channel and never any client surface
// (D5/R4). Unknown models are refused; nothing is hinted back.
func (s *Service) handleGWVerifyModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	presets, err := s.db.ListModelPresets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	for _, p := range presets {
		if (req.ID != "" && p.ID == req.ID) ||
			(req.ID == "" && req.Provider != "" && p.Provider == req.Provider && p.Model == req.Model) {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": p.ID, "name": p.Name, "provider": p.Provider, "model": p.Model,
				"max_tokens": p.MaxTokens, "temperature": p.Temperature, "base_url": p.BaseURL,
				"api_key": s.settings.GetSecret(wocadb.ModelPresetKeySecret(p.ID)),
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown model"})
}

// notifyConfig publishes the new generation after every write so
// subscribed roles rebuild immediately instead of waiting for the poll.
func (s *Service) notifyConfig() {
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

func (s *Service) handleListManagedSkills(w http.ResponseWriter, _ *http.Request) {
	skillsList := s.managedSkillsPayload()
	if skillsList == nil {
		skillsList = []ManagedSkill{}
	}
	writeJSON(w, http.StatusOK, skillsList)
}

func (s *Service) handlePutManagedSkill(w http.ResponseWriter, r *http.Request) {
	var req ManagedSkill
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}
	if len(req.Name) > 64 || len(req.Body) > 128<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name ≤64 chars, body ≤128KiB"})
		return
	}
	now := wocadb.Now()
	if _, err := s.db.Exec(
		`INSERT INTO managed_skills (name, description, body, updated_at) VALUES (`+phI(s.db, 0)+`, `+phI(s.db, 1)+`, `+phI(s.db, 2)+`, `+phI(s.db, 3)+`)
		 ON CONFLICT(name) DO UPDATE SET description = excluded.description, body = excluded.body, updated_at = excluded.updated_at`,
		req.Name, req.Description, req.Body, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.log.Info().Str("skill", req.Name).Msg("managed skill saved via config plane")
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleDeleteManagedSkill(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`DELETE FROM managed_skills WHERE name = `+phI(s.db, 0), r.PathValue("name"))
	if err != nil || nRows(res) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown skill"})
		return
	}
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleListManagedMCP(w http.ResponseWriter, _ *http.Request) {
	servers := s.managedMCPPayload()
	if servers == nil {
		servers = []ManagedMCP{}
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Service) handlePutManagedMCP(w http.ResponseWriter, r *http.Request) {
	var req ManagedMCP
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Spec = strings.TrimSpace(req.Spec)
	if req.Name == "" || req.Spec == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and spec are required"})
		return
	}
	// The spec grammar the bridges consume: name=command (stdio) or
	// name=http(s)://endpoint (streamable HTTP). The stored spec must
	// carry its own name prefix or start with it — normalize to
	// name=<rest>.
	rest := strings.TrimSpace(strings.TrimPrefix(req.Spec, req.Name+"="))
	if rest == "" || (!strings.HasPrefix(rest, "http://") && !strings.HasPrefix(rest, "https://") && !strings.Contains(rest, " ")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "spec must be a command or an http(s):// endpoint"})
		return
	}
	now := wocadb.Now()
	if _, err := s.db.Exec(
		`INSERT INTO managed_mcp (name, spec, updated_at) VALUES (`+phI(s.db, 0)+`, `+phI(s.db, 1)+`, `+phI(s.db, 2)+`)
		 ON CONFLICT(name) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`,
		req.Name, req.Name+"="+rest, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.log.Info().Str("mcp", req.Name).Msg("managed mcp saved via config plane")
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleDeleteManagedMCP(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`DELETE FROM managed_mcp WHERE name = `+phI(s.db, 0), r.PathValue("name"))
	if err != nil || nRows(res) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown mcp server"})
		return
	}
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func phI(db *wocadb.DB, i int) string { return db.Dialect.Placeholder(i) }

func nRows(res sql.Result) int64 {
	n, _ := res.RowsAffected()
	return n
}

// requireScope wraps h with the route's scope check (gateway.RequireScope).
func (s *Service) requireScope(scope string, h http.HandlerFunc) http.Handler {
	return gateway.RequireScope(authz.NewLocal(), scope)(h)
}

// payload mirrors the business settings API's JSON contract. It is
// deliberately a local definition, not a shared struct: the two planes are
// separate services whose wire contract must stay explicit and versioned
// independently (design §16.2).
type payload struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	MaxTokens             int    `json:"max_tokens"`
	Temperature           string `json:"temperature"`
	AnthropicBaseURL      string `json:"anthropic_base_url"`
	OpenAIBaseURL         string `json:"openai_base_url"`
	GLMBaseURL            string `json:"glm_base_url"`
	DeepSeekBaseURL       string `json:"deepseek_base_url"`
	CheckpointBackend     string `json:"checkpoint_backend"`
	MemoryBackend         string `json:"memory_backend"`
	CheckpointDir         string `json:"checkpoint_dir"`
	Database              string `json:"database"`
	DebugLog              bool   `json:"debug_log"`
	MemoryEngine          string `json:"memory_engine"`
	LadyMDBPath           string `json:"ladym_db_path"`
	LadyMEmbedding        string `json:"ladym_embedding"`
	LadyMEmbeddingModel   string `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL string `json:"ladym_embedding_base_url"`
	LadyMLLM              string `json:"ladym_llm"`
	LadyMAutoRetain       bool   `json:"ladym_autoretain"`
	LadyMTopK             int    `json:"ladym_topk"`
	LadyMSystem2          bool   `json:"ladym_system2"`
	LadyMSystem2Interval  int    `json:"ladym_system2_interval"`

	LadyMEndpoint       string `json:"ladym_endpoint"`
	LadyMAPIUsername    string `json:"ladym_api_username"`
	PostgresURLSet      bool   `json:"postgres_url_set"`
	LadyMAPIPasswordSet bool   `json:"ladym_api_password_set"`

	CheckpointPGHost        string `json:"checkpoint_pg_host"`
	CheckpointPGPort        string `json:"checkpoint_pg_port"`
	CheckpointPGDB          string `json:"checkpoint_pg_db"`
	CheckpointPGUser        string `json:"checkpoint_pg_user"`
	CheckpointPGPasswordSet bool   `json:"checkpoint_pg_password_set"`
	MemoryPGHost            string `json:"memory_pg_host"`
	MemoryPGPort            string `json:"memory_pg_port"`
	MemoryPGDB              string `json:"memory_pg_db"`
	MemoryPGUser            string `json:"memory_pg_user"`
	MemoryPGPasswordSet     bool   `json:"memory_pg_password_set"`

	MsgPlaneMode    string `json:"msg_plane_mode"`
	MsgPlaneNATSURL string `json:"msg_plane_nats_url"`
	MsgPlaneSubject string `json:"msg_plane_subject"`
	// MsgPlaneAvailable is always true here: the config plane manages the
	// keys for whatever deployment form reads this database (the business
	// server reports its own dispatch availability separately).
	MsgPlaneAvailable bool `json:"msg_plane_available"`
}

// update is the PUT body. Pointer fields distinguish "unchanged" (absent)
// from "clear" (present but empty).
type update struct {
	Provider              *string  `json:"provider"`
	Model                 *string  `json:"model"`
	MaxTokens             *int     `json:"max_tokens"`
	Temperature           *float64 `json:"temperature"`
	AnthropicBaseURL      *string  `json:"anthropic_base_url"`
	OpenAIBaseURL         *string  `json:"openai_base_url"`
	GLMBaseURL            *string  `json:"glm_base_url"`
	DeepSeekBaseURL       *string  `json:"deepseek_base_url"`
	AnthropicAPIKey       *string  `json:"anthropic_api_key"`
	OpenAIAPIKey          *string  `json:"openai_api_key"`
	GLMAPIKey             *string  `json:"glm_api_key"`
	DeepSeekAPIKey        *string  `json:"deepseek_api_key"`
	CheckpointBackend     *string  `json:"checkpoint_backend"`
	MemoryBackend         *string  `json:"memory_backend"`
	CheckpointDir         *string  `json:"checkpoint_dir"`
	DebugLog              *bool    `json:"debug_log"`
	MemoryEngine          *string  `json:"memory_engine"`
	LadyMDBPath           *string  `json:"ladym_db_path"`
	LadyMEmbedding        *string  `json:"ladym_embedding"`
	LadyMEmbeddingModel   *string  `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL *string  `json:"ladym_embedding_base_url"`
	LadyMEmbeddingAPIKey  *string  `json:"ladym_embedding_api_key"`
	LadyMLLM              *string  `json:"ladym_llm"`
	LadyMAutoRetain       *bool    `json:"ladym_autoretain"`
	LadyMTopK             *int     `json:"ladym_topk"`
	LadyMSystem2          *bool    `json:"ladym_system2"`
	LadyMSystem2Interval  *int     `json:"ladym_system2_interval"`

	LadyMEndpoint    *string `json:"ladym_endpoint"`
	LadyMAPIUsername *string `json:"ladym_api_username"`
	PostgresURL      *string `json:"postgres_url"`
	LadyMAPIPassword *string `json:"ladym_api_password"`

	CheckpointPGHost     *string `json:"checkpoint_pg_host"`
	CheckpointPGPort     *string `json:"checkpoint_pg_port"`
	CheckpointPGDB       *string `json:"checkpoint_pg_db"`
	CheckpointPGUser     *string `json:"checkpoint_pg_user"`
	CheckpointPGPassword *string `json:"checkpoint_pg_password"`
	MemoryPGHost         *string `json:"memory_pg_host"`
	MemoryPGPort         *string `json:"memory_pg_port"`
	MemoryPGDB           *string `json:"memory_pg_db"`
	MemoryPGUser         *string `json:"memory_pg_user"`
	MemoryPGPassword     *string `json:"memory_pg_password"`

	MsgPlaneMode    *string `json:"msg_plane_mode"`
	MsgPlaneNATSURL *string `json:"msg_plane_nats_url"`
	MsgPlaneSubject *string `json:"msg_plane_subject"`

	// BuiltinPlugins pins the enterprise plugin set delivered to bridges
	// (present = pin; absent = user-managed on each bridge).
	BuiltinPlugins *[]string `json:"builtin_plugins"`
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

type patch struct {
	key   string
	value *string
}

func intPtr(p *int) *string {
	if p == nil {
		return nil
	}
	return ptr(strconv.Itoa(*p))
}

func floatPtr(p *float64) *string {
	if p == nil {
		return nil
	}
	return ptr(strconv.FormatFloat(*p, 'f', -1, 64))
}

func (u *update) boolPtr(p *bool) *string {
	if p == nil {
		return nil
	}
	return ptr(strconv.FormatBool(*p))
}

func ptr(s string) *string { return &s }

func (s *Service) handleGet(w http.ResponseWriter, _ *http.Request) {
	cfg := s.settings.Load()
	writeJSON(w, http.StatusOK, payload{
		Provider:                cfg.Provider,
		Model:                   cfg.Model,
		MaxTokens:               cfg.MaxTokens,
		Temperature:             formatFloat(cfg.Temperature),
		AnthropicBaseURL:        cfg.AnthropicBaseURL,
		OpenAIBaseURL:           cfg.OpenAIBaseURL,
		GLMBaseURL:              cfg.GLMBaseURL,
		DeepSeekBaseURL:         cfg.DeepSeekBaseURL,
		CheckpointBackend:       cfg.CheckpointBackend,
		MemoryBackend:           cfg.MemoryBackend,
		CheckpointDir:           cfg.CheckpointDir,
		Database:                s.settings.Path(),
		DebugLog:                cfg.DebugLog,
		MemoryEngine:            cfg.MemoryEngine,
		LadyMDBPath:             cfg.LadyMDBPath,
		LadyMEmbedding:          cfg.LadyMEmbedding,
		LadyMEmbeddingModel:     cfg.LadyMEmbeddingModel,
		LadyMEmbeddingBaseURL:   cfg.LadyMEmbeddingBaseURL,
		LadyMLLM:                cfg.LadyMLLM,
		LadyMAutoRetain:         cfg.LadyMAutoRetain,
		LadyMTopK:               cfg.LadyMTopK,
		LadyMSystem2:            cfg.LadyMSystem2,
		LadyMSystem2Interval:    cfg.LadyMSystem2Interval,
		LadyMEndpoint:           cfg.LadyMEndpoint,
		LadyMAPIUsername:        cfg.LadyMAPIUsername,
		PostgresURLSet:          s.settings.HasSecret(settings.SecretPostgresURL),
		LadyMAPIPasswordSet:     s.settings.HasSecret(settings.SecretLadyMAPIPassword),
		CheckpointPGHost:        cfg.CheckpointPGHost,
		CheckpointPGPort:        orDefault(cfg.CheckpointPGPort, "5432"),
		CheckpointPGDB:          orDefault(cfg.CheckpointPGDB, "woca"),
		CheckpointPGUser:        cfg.CheckpointPGUser,
		CheckpointPGPasswordSet: s.settings.HasSecret(settings.SecretCheckpointPGPassword),
		MemoryPGHost:            cfg.MemoryPGHost,
		MemoryPGPort:            orDefault(cfg.MemoryPGPort, "5432"),
		MemoryPGDB:              orDefault(cfg.MemoryPGDB, "woca"),
		MemoryPGUser:            cfg.MemoryPGUser,
		MemoryPGPasswordSet:     s.settings.HasSecret(settings.SecretMemoryPGPassword),
		MsgPlaneMode:            orDefault(cfg.MsgPlaneMode, "pg"),
		MsgPlaneNATSURL:         cfg.MsgPlaneNATSURL,
		MsgPlaneSubject:         orDefault(cfg.MsgPlaneSubject, "runs.dispatch"),
		// The config plane manages the keys for every deployment form that
		// reads this database; availability reporting belongs to the planes
		// that know their own topology (the business server reports its own
		// dispatch state).
		MsgPlaneAvailable: true,
	})
}

// handleUpdate applies an update: settings keys through Apply (enum and
// range validation included), secrets through the encrypted store. The
// response reports which effect classes the change triggers so operators
// see what a restart vs a live rebuild means; the live rebuild itself
// happens in the business roles via the settings-generation watcher.
func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var u update
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.settings.Load()
	for _, p := range u.patches() {
		// The enterprise config plane only offers postgres-grade
		// persistence (product ruling: no embedded sqlite/file/memory
		// stores behind the enterprise console) — reject anything else
		// before it reaches the settings tables.
		if (p.key == settings.KeyCheckpoint || p.key == settings.KeyMemory) && *p.value != "postgres" {
			writeError(w, http.StatusBadRequest, "the enterprise config plane only supports the postgres backend for checkpoint and memory (got "+*p.value+")")
			return
		}
		if err := settings.Apply(&cfg, p.key, *p.value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range u.secretPatches() {
		if p.value == nil {
			continue
		}
		if err := s.settings.SetSecret(p.key, *p.value); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.log.Info().Strs("keys", updateKeys(&u)).Msg("settings updated via config plane")
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// exportFile is the declarative config document (design §16.4): the plain
// settings values plus, optionally, decrypted secrets for Git-safe full
// restores.
type exportFile struct {
	Values  map[string]string `json:"values"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

func (s *Service) handleExport(w http.ResponseWriter, r *http.Request) {
	cfg := s.settings.Load()
	values := map[string]string{}
	for _, k := range settings.KeyNames() {
		if v := settings.KeyValue(cfg, k); v != "" {
			values[k] = v
		}
	}
	out := exportFile{Values: values}
	if r.URL.Query().Get("include_secrets") == "true" {
		secrets := map[string]string{}
		for _, name := range []string{
			settings.SecretAnthropicAPIKey, settings.SecretOpenAIAPIKey,
			settings.SecretGLMAPIKey, settings.SecretDeepSeekAPIKey,
			settings.SecretPostgresURL, settings.SecretLadyMEmbeddingAPIKey,
			settings.SecretCheckpointPGPassword, settings.SecretMemoryPGPassword,
		} {
			if v := s.settings.GetSecret(name); v != "" {
				secrets[name] = v
			}
		}
		if len(secrets) > 0 {
			out.Secrets = secrets
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleImport idempotently applies an export document: every values entry
// through Apply (so invalid documents fail atomically before any write),
// then secrets. An empty document is a no-op.
func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	var doc exportFile
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.settings.Load()
	for k, v := range doc.Values {
		if _, isSecret := settings.SecretFor(k); isSecret {
			writeError(w, http.StatusBadRequest, k+" is a secret; put it in the secrets section")
			return
		}
		if err := settings.Apply(&cfg, k, v); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for name, v := range doc.Secrets {
		if err := s.settings.SetSecret(name, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.log.Info().Int("values", len(doc.Values)).Int("secrets", len(doc.Secrets)).Msg("settings imported via config plane")
	s.notifyConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "values": len(doc.Values), "secrets": len(doc.Secrets)})
}

// ackStaleAfter bounds how fresh an ack must be to count as live
// (design §16.7: the ack row doubles as the heartbeat).
const ackStaleAfter = int64(15_000)

// handleStatus reports the distribution inventory (design §16.7): the
// global settings + tenant generations and every service's self-reported
// ack, so operators see who received what — and who fell behind.
func (s *Service) handleStatus(w http.ResponseWriter, _ *http.Request) {
	gen := s.settings.Generation()
	tenantGen := s.tenants.Generation()
	resp := map[string]any{
		"generation":        gen,
		"tenant_generation": tenantGen,
		"services":          []map[string]any{},
	}
	if s.db != nil {
		// Janitor pass: acks from dead container generations (hostnames
		// churn on every recreate) age out of the inventory after ten
		// minutes — well past the 15s liveness window, while a restarting
		// role re-arms its ack within seconds of boot.
		now := wocadb.Now()
		if err := s.db.PruneConfigAcks(now - 600_000); err != nil {
			s.log.Warn().Err(err).Msg("config ack prune failed")
		}
		acks, err := s.db.ConfigAcks()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sort.Slice(acks, func(i, j int) bool { return acks[i].ServiceID < acks[j].ServiceID })
		services := make([]map[string]any, 0, len(acks))
		for _, a := range acks {
			services = append(services, map[string]any{
				"service_id":     a.ServiceID,
				"role":           a.Role,
				"generation":     a.Generation,
				"addr":           a.Addr,
				"updated_at":     a.UpdatedAt,
				"live":           now-a.UpdatedAt < ackStaleAfter,
				"config_lagging": a.Generation != gen,
			})
		}
		resp["services"] = services
	}
	writeJSON(w, http.StatusOK, resp)
}

// modelPayload is one preset over the wire. KeySet reports whether the
// encrypted key secret exists (the key itself never leaves the store).
type modelPayload struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	MaxTokens   int64   `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	BaseURL     string  `json:"base_url"`
	KeySet      bool    `json:"key_set"`
	UpdatedAt   int64   `json:"updated_at"`
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
