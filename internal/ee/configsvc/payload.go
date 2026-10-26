// Package configsvc is the standalone settings management plane (design
// §16): a dedicated role that owns configuration read/write, validation,
// export/import, and its console SPA. Business roles (woca-scheduler,
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
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/projanvil/woca/internal/settings"
)

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
	// The Langfuse observability plugin rides when the console enabled it
	// (its secret travels via ManagedSecretKeys below).
	if cfg.LangfuseEnabled {
		kv["langfuse-enabled"] = "true"
		kv["langfuse-base-url"] = cfg.LangfuseBaseURL
		kv["langfuse-public-key"] = cfg.LangfusePublicKey
	}
	for key, secret := range settings.ManagedSecretKeys {
		if secret == settings.SecretLangfuseSecretKey && !cfg.LangfuseEnabled {
			continue // the plugin's secret only rides when the console enabled it
		}
		if v := s.settings.GetSecret(secret); v != "" {
			kv[key] = v
		}
	}
	// Operator-delivered skills & MCP (§16): they ride the package and
	// materialize as the bridge's lowest merge layer; user config still
	// wins.
	skillsList, err := s.managedSkillsPayload()
	if err != nil {
		return "", nil, err
	}
	if len(skillsList) > 0 {
		if raw, err := json.Marshal(skillsList); err == nil {
			kv["managed-skills"] = string(raw)
		}
	}
	servers, err := s.managedMCPPayload()
	if err != nil {
		return "", nil, err
	}
	if len(servers) > 0 {
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

// managedSkillsPayload reads the delivered-skill inventory. A read error
// propagates: silently serving a partial inventory would tell every bridge
// "these are all the managed skills" — removals would propagate as though
// the operator made them.
func (s *Service) managedSkillsPayload() ([]ManagedSkill, error) {
	rows, err := s.db.Query(`SELECT name, description, body, updated_at FROM managed_skills ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("configsvc: managed skills read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ManagedSkill
	for rows.Next() {
		var sk ManagedSkill
		if err := rows.Scan(&sk.Name, &sk.Description, &sk.Body, &sk.UpdatedAt); err != nil {
			return nil, fmt.Errorf("configsvc: managed skills row: %w", err)
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// managedMCPPayload reads the delivered-MCP inventory; same error
// contract as managedSkillsPayload (a partial inventory must not ship as
// the complete set).
func (s *Service) managedMCPPayload() ([]ManagedMCP, error) {
	rows, err := s.db.Query(`SELECT name, spec, updated_at FROM managed_mcp ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("configsvc: managed mcp read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ManagedMCP
	for rows.Next() {
		var m ManagedMCP
		if err := rows.Scan(&m.Name, &m.Spec, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("configsvc: managed mcp row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
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

	// Langfuse observability plugin — enterprise-owned: configured here,
	// delivered to business roles via the shared settings tables and to
	// bridges via the config package.
	LangfuseEnabled      bool   `json:"langfuse_enabled"`
	LangfuseBaseURL      string `json:"langfuse_base_url"`
	LangfusePublicKey    string `json:"langfuse_public_key"`
	LangfuseSecretKeySet bool   `json:"langfuse_secret_key_set"`

	// Web search — the enterprise default chain. A non-empty provider list
	// pins the exact allowed set on every managed machine; the endpoints are
	// the ones that need one. The custom endpoint's bearer key is write-only
	// (search_custom_key_set only reports whether one is stored).
	SearchProviders       []string `json:"search_providers"`
	SearchSearXNGEndpoint string   `json:"search_searxng_endpoint"`
	SearchCustomEndpoint  string   `json:"search_custom_endpoint"`
	SearchCustomKeySet    bool     `json:"search_custom_key_set"`
	SearchAllowedDomains  []string `json:"search_allowed_domains"`
	SearchDeniedDomains   []string `json:"search_denied_domains"`
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

	LangfuseEnabled   *bool   `json:"langfuse_enabled"`
	LangfuseBaseURL   *string `json:"langfuse_base_url"`
	LangfusePublicKey *string `json:"langfuse_public_key"`
	LangfuseSecretKey *string `json:"langfuse_secret_key"`

	// Web search. Present pins the value on managed machines; absent leaves
	// it to the user. Lists are replaced wholesale, not merged.
	SearchProviders       *[]string `json:"search_providers"`
	SearchSearXNGEndpoint *string   `json:"search_searxng_endpoint"`
	SearchCustomEndpoint  *string   `json:"search_custom_endpoint"`
	SearchAllowedDomains  *[]string `json:"search_allowed_domains"`
	SearchDeniedDomains   *[]string `json:"search_denied_domains"`

	SearchCustomKey *string `json:"search_custom_key"`
}

type patch struct {
	key   string
	value *string
}

func intPtr(p *int) *string {
	if p == nil {
		return nil
	}
	return new(strconv.Itoa(*p))
}

func floatPtr(p *float64) *string {
	if p == nil {
		return nil
	}
	return new(strconv.FormatFloat(*p, 'f', -1, 64))
}

func (u *update) boolPtr(p *bool) *string {
	if p == nil {
		return nil
	}
	return new(strconv.FormatBool(*p))
}

// jsonListPtr renders a list patch as the JSON array string settings.Apply
// expects. A nil pointer means "not in the request" and is dropped; a non-nil
// empty list means "clear".
func jsonListPtr(p *[]string) *string {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(*p)
	if err != nil {
		return nil
	}
	return new(string(raw))
}
