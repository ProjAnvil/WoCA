// Package settings persists woca's user-facing configuration (provider,
// model, endpoints, storage backends) in the woca database, with API keys and
// DSNs encrypted at rest via AES-256-GCM.
package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

// Checkpoint backends selectable for graph checkpoint persistence.
const (
	CheckpointNone     = "none"
	CheckpointFile     = "file"
	CheckpointSQLite   = "sqlite"
	CheckpointPostgres = "postgres"
)

// Memory-store backends.
const (
	MemoryNone     = "none"
	MemoryInMem    = "memory"
	MemorySQLite   = "sqlite"
	MemoryPostgres = "postgres"
)

// Memory-engine selectors (the ladyM factual-memory subsystem).
const (
	MemoryEngineLadyM  = "ladym"
	MemoryEngineNone   = "none"
	MemoryEngineRemote = "remote"
)

// ladyM embedding providers.
const (
	LadyMEmbeddingInherit = "inherit"
	LadyMEmbeddingHashing = "hashing"
	LadyMEmbeddingOpenAI  = "openai"
	LadyMEmbeddingOllama  = "ollama"
	LadyMEmbeddingHTTP    = "http"
)

// ladyM LLM usage for offline consolidation.
const (
	LadyMLLMInherit = "inherit"
	LadyMLLMNone    = "none"
)

// ladyM numeric defaults and bounds.
const (
	DefaultLadyMTopK            = 6
	DefaultLadyMSystem2Interval = 300
	MaxLadyMTopK                = 20
	MinLadyMSystem2Interval     = 60
)

// MaxThinkingBudget mirrors config.MaxThinkingBudget (the Anthropic extended
// thinking ceiling semantics of MAX_THINKING_TOKENS).
const MaxThinkingBudget = 31999

// Secret names stored encrypted in the secrets table.
const (
	SecretAnthropicAPIKey      = "anthropic_api_key"
	SecretOpenAIAPIKey         = "openai_api_key"
	SecretGLMAPIKey            = "glm_api_key"
	SecretDeepSeekAPIKey       = "deepseek_api_key"
	SecretPostgresURL          = "postgres_url"
	SecretLadyMEmbeddingAPIKey = "ladym_embedding_api_key"
	SecretLadyMAPIPassword     = "ladym_api_password"
	SecretCheckpointPGPassword = "checkpoint_pg_password"
	SecretMemoryPGPassword     = "memory_pg_password"
)

// Settings is the persisted configuration. Secrets (API keys, the postgres
// DSN) are deliberately not part of this struct; they live in the secrets
// table and are accessed via Store secrets methods.
type Settings struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	// ThinkingBudget is the extended-thinking budget in tokens (0 = off).
	// Switchable at runtime from the chat UI; the provider template maps it
	// to budget_tokens / reasoning_effort / a plain thinking switch.
	ThinkingBudget        int    `json:"thinking_budget"`
	AnthropicBaseURL      string `json:"anthropic_base_url"`
	OpenAIBaseURL         string `json:"openai_base_url"`
	GLMBaseURL            string `json:"glm_base_url"`
	DeepSeekBaseURL       string `json:"deepseek_base_url"`
	CheckpointBackend     string `json:"checkpoint_backend"`
	MemoryBackend         string `json:"memory_backend"`
	CheckpointDir         string `json:"checkpoint_dir"`
	OutputStyle           string `json:"output_style"`
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
	LadyMEndpoint         string `json:"ladym_endpoint"`
	LadyMAPIUsername      string `json:"ladym_api_username"`
	DebugLog              bool   `json:"debug_log"`
	// Message plane (run dispatch): "pg" (default) keeps the PG queue
	// alone; "pg+nats" adds NATS dispatch notifications on top. Live
	// reload — resolved per operation by internal/msgplane.
	MsgPlaneMode    string `json:"msg_plane_mode"`
	MsgPlaneNATSURL string `json:"msg_plane_nats_url"`
	MsgPlaneSubject string `json:"msg_plane_subject"`
	// Per-subsystem postgres connection parts (design: the console
	// collects host/port/db/user as plain rows and only the password as a
	// secret; the consumer assembles the DSN). Empty host + a legacy
	// postgres_url secret falls back to that DSN.
	CheckpointPGHost string `json:"checkpoint_pg_host"`
	CheckpointPGPort string `json:"checkpoint_pg_port"`
	CheckpointPGDB   string `json:"checkpoint_pg_db"`
	CheckpointPGUser string `json:"checkpoint_pg_user"`
	MemoryPGHost     string `json:"memory_pg_host"`
	MemoryPGPort     string `json:"memory_pg_port"`
	MemoryPGDB       string `json:"memory_pg_db"`
	MemoryPGUser     string `json:"memory_pg_user"`
	// ClaudeSkills lists the ~/.claude skill names enabled for the agent
	// (checkbox selection in settings; JSON array in the DB).
	ClaudeSkills []string `json:"claude_skills"`
	// ClaudeMCP lists the ~/.claude.json global MCP server names enabled at
	// agent startup.
	ClaudeMCP []string `json:"claude_mcp"`
	// BuiltinPlugins lists the built-in capability plugins (woca-plugins/
	// tree: browser, pdf) whose agent tools are enabled.
	BuiltinPlugins []string `json:"builtin_plugins"`
}

// Default returns the default settings: sqlite storage under ~/.woca.
func Default() Settings {
	return Settings{
		Provider:          "anthropic",
		CheckpointBackend: CheckpointSQLite,
		MemoryBackend:     MemorySQLite,
		MemoryEngine:      MemoryEngineLadyM,
		LadyMAutoRetain:   true,
	}
}

// Normalize fills empty fields with defaults and validates backend names.
func (s Settings) Normalize() (Settings, error) {
	out := s
	if out.Provider == "" {
		out.Provider = "anthropic"
	}
	switch out.Provider {
	case "anthropic", "openai", "glm", "deepseek":
	default:
		return out, fmt.Errorf("settings: unknown provider %q", out.Provider)
	}
	switch out.CheckpointBackend {
	case "", CheckpointNone:
		out.CheckpointBackend = CheckpointNone
	case CheckpointFile, CheckpointSQLite, CheckpointPostgres:
	default:
		return out, fmt.Errorf("settings: unknown checkpoint backend %q", out.CheckpointBackend)
	}
	switch out.MemoryBackend {
	case "", MemoryNone:
		out.MemoryBackend = MemoryNone
	case MemoryInMem, MemorySQLite, MemoryPostgres:
	default:
		return out, fmt.Errorf("settings: unknown memory backend %q", out.MemoryBackend)
	}
	switch out.MemoryEngine {
	case "":
		out.MemoryEngine = MemoryEngineLadyM
	case MemoryEngineLadyM, MemoryEngineNone, MemoryEngineRemote:
	default:
		return out, fmt.Errorf("settings: unknown memory engine %q", out.MemoryEngine)
	}
	switch out.LadyMEmbedding {
	case "":
		out.LadyMEmbedding = LadyMEmbeddingInherit
	case LadyMEmbeddingInherit, LadyMEmbeddingHashing, LadyMEmbeddingOpenAI, LadyMEmbeddingOllama, LadyMEmbeddingHTTP:
	default:
		return out, fmt.Errorf("settings: unknown ladym embedding %q", out.LadyMEmbedding)
	}
	switch out.LadyMLLM {
	case "":
		out.LadyMLLM = LadyMLLMInherit
	case LadyMLLMInherit, LadyMLLMNone:
	default:
		return out, fmt.Errorf("settings: unknown ladym llm %q", out.LadyMLLM)
	}
	if out.LadyMTopK == 0 {
		out.LadyMTopK = DefaultLadyMTopK
	}
	if out.LadyMSystem2Interval == 0 {
		out.LadyMSystem2Interval = DefaultLadyMSystem2Interval
	}
	return out, nil
}

// Store reads and writes settings plus encrypted secrets in a woca database.
type Store struct {
	db      *wocadb.DB
	crypto  *wocadb.Crypto
	managed managedState
}

// SetManaged installs the enterprise managed-settings overlay: its values
// override database reads and are locked against writes.
func (s *Store) SetManaged(m Settings) {
	if s == nil {
		return
	}
	s.managed.mu.Lock()
	defer s.managed.mu.Unlock()
	if reflect.DeepEqual(m, Settings{}) {
		s.managed.overlay = nil
		return
	}
	s.managed.overlay = &m
}

// Open opens the database at target (see wocadb.Open) and prepares the
// secrets crypto key. It returns a nil Store when target is disabled.
func Open(target string) (*Store, error) {
	db, err := wocadb.Open(target)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	crypto, err := wocadb.LoadOrCreateCrypto(wocadb.KeyPath())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, crypto: crypto}, nil
}

// Generation returns a cheap change fingerprint of the settings stores:
// the summed updated_at of every settings and secrets row. The config
// plane is the sole writer, so business roles compare this value to detect
// config-plane writes and re-run their build hooks (internal/settingswatch).
// The empty string means "no database".
func (s *Store) Generation() string {
	if s == nil || s.db == nil {
		return ""
	}
	var gen string
	// Two single-row scans; failures yield the zero fingerprint (the
	// watcher then simply sees no change).
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(updated_at), 0) FROM settings`).Scan(&gen)
	var sec string
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(updated_at), 0) FROM secrets`).Scan(&sec)
	return gen + "/" + sec
}

// OpenFromDB builds a store over an already-opened wocadb (sharing the
// connection pool and crypto material with the caller's database).
func OpenFromDB(db *wocadb.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("settings: nil database")
	}
	crypto, err := wocadb.LoadOrCreateCrypto(wocadb.KeyPath())
	if err != nil {
		return nil, err
	}
	return &Store{db: db, crypto: crypto}, nil
}

// DB exposes the underlying database (used by checkpoint/memory backends).
func (s *Store) DB() *wocadb.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path describes the database location for display.
func (s *Store) Path() string {
	if s == nil || s.db == nil {
		return "disabled"
	}
	return string(s.db.Dialect) + ":" + s.db.Path
}

// Load reads all settings keys; missing keys fall back to Default.
func (s *Store) Load() Settings {
	out := Default()
	if s == nil || s.db == nil {
		return out
	}
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	pairs := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			pairs[k] = v
		}
	}
	applyPairs(&out, pairs)
	if n, err := out.Normalize(); err == nil {
		out = n
	}
	s.managed.applyOverlay(&out)
	return out
}

func applyPairs(out *Settings, pairs map[string]string) {
	if v, ok := pairs["provider"]; ok {
		out.Provider = v
	}
	if v, ok := pairs["model"]; ok {
		out.Model = v
	}
	if v, ok := pairs["max_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.MaxTokens = n
		}
	}
	if v, ok := pairs["temperature"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out.Temperature = f
		}
	}
	if v, ok := pairs["thinking_budget"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.ThinkingBudget = n
		}
	}
	if v, ok := pairs["anthropic_base_url"]; ok {
		out.AnthropicBaseURL = v
	}
	if v, ok := pairs["openai_base_url"]; ok {
		out.OpenAIBaseURL = v
	}
	if v, ok := pairs["glm_base_url"]; ok {
		out.GLMBaseURL = v
	}
	if v, ok := pairs["deepseek_base_url"]; ok {
		out.DeepSeekBaseURL = v
	}
	if v, ok := pairs["claude_skills"]; ok {
		out.ClaudeSkills = parseNameList(v)
	}
	if v, ok := pairs["claude_mcp"]; ok {
		out.ClaudeMCP = parseNameList(v)
	}
	if v, ok := pairs["builtin_plugins"]; ok {
		out.BuiltinPlugins = parseNameList(v)
	}
	if v, ok := pairs["checkpoint_backend"]; ok {
		out.CheckpointBackend = v
	}
	if v, ok := pairs["memory_backend"]; ok {
		out.MemoryBackend = v
	}
	if v, ok := pairs["checkpoint_dir"]; ok {
		out.CheckpointDir = v
	}
	if v, ok := pairs["output_style"]; ok {
		out.OutputStyle = v
	}
	if v, ok := pairs["debug_log"]; ok {
		out.DebugLog, _ = ParseBool(v)
	}
	if v, ok := pairs["msg_plane_mode"]; ok {
		out.MsgPlaneMode = v
	}
	if v, ok := pairs["msg_plane_nats_url"]; ok {
		out.MsgPlaneNATSURL = v
	}
	if v, ok := pairs["msg_plane_subject"]; ok {
		out.MsgPlaneSubject = v
	}
	if v, ok := pairs["checkpoint_pg_host"]; ok {
		out.CheckpointPGHost = v
	}
	if v, ok := pairs["checkpoint_pg_port"]; ok {
		out.CheckpointPGPort = v
	}
	if v, ok := pairs["checkpoint_pg_db"]; ok {
		out.CheckpointPGDB = v
	}
	if v, ok := pairs["checkpoint_pg_user"]; ok {
		out.CheckpointPGUser = v
	}
	if v, ok := pairs["memory_pg_host"]; ok {
		out.MemoryPGHost = v
	}
	if v, ok := pairs["memory_pg_port"]; ok {
		out.MemoryPGPort = v
	}
	if v, ok := pairs["memory_pg_db"]; ok {
		out.MemoryPGDB = v
	}
	if v, ok := pairs["memory_pg_user"]; ok {
		out.MemoryPGUser = v
	}
	if v, ok := pairs["memory_engine"]; ok {
		out.MemoryEngine = v
	}
	if v, ok := pairs["ladym_db_path"]; ok {
		out.LadyMDBPath = v
	}
	if v, ok := pairs["ladym_embedding"]; ok {
		out.LadyMEmbedding = v
	}
	if v, ok := pairs["ladym_embedding_model"]; ok {
		out.LadyMEmbeddingModel = v
	}
	if v, ok := pairs["ladym_embedding_base_url"]; ok {
		out.LadyMEmbeddingBaseURL = v
	}
	if v, ok := pairs["ladym_llm"]; ok {
		out.LadyMLLM = v
	}
	if v, ok := pairs["ladym_autoretain"]; ok {
		out.LadyMAutoRetain, _ = ParseBool(v)
	}
	if v, ok := pairs["ladym_topk"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.LadyMTopK = n
		}
	}
	if v, ok := pairs["ladym_system2"]; ok {
		out.LadyMSystem2, _ = ParseBool(v)
	}
	if v, ok := pairs["ladym_system2_interval"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.LadyMSystem2Interval = n
		}
	}
	if v, ok := pairs["ladym_endpoint"]; ok {
		out.LadyMEndpoint = v
	}
	if v, ok := pairs["ladym_api_username"]; ok {
		out.LadyMAPIUsername = v
	}
}

// Save persists non-empty settings fields (empty fields are left unchanged so
// partial updates from the UI don't wipe prior values; use Clear to remove).
func (s *Store) Save(cfg Settings) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("settings: store disabled")
	}
	if err := s.managed.checkWritable(cfg); err != nil {
		return err
	}
	type field struct{ key, val string }
	fields := []field{
		{"provider", cfg.Provider},
		{"model", cfg.Model},
		{"anthropic_base_url", cfg.AnthropicBaseURL},
		{"openai_base_url", cfg.OpenAIBaseURL},
		{"glm_base_url", cfg.GLMBaseURL},
		{"deepseek_base_url", cfg.DeepSeekBaseURL},
		{"checkpoint_backend", cfg.CheckpointBackend},
		{"memory_backend", cfg.MemoryBackend},
		{"checkpoint_dir", cfg.CheckpointDir},
		{"output_style", cfg.OutputStyle},
		{"memory_engine", cfg.MemoryEngine},
		{"ladym_db_path", cfg.LadyMDBPath},
		{"ladym_embedding", cfg.LadyMEmbedding},
		{"ladym_embedding_model", cfg.LadyMEmbeddingModel},
		{"ladym_embedding_base_url", cfg.LadyMEmbeddingBaseURL},
		{"ladym_llm", cfg.LadyMLLM},
	}
	// Claude selections are always written explicitly (an empty selection
	// persists as "[]") so unchecking everything sticks.
	fields = append(fields, field{"claude_skills", FormatNameList(cfg.ClaudeSkills)})
	fields = append(fields, field{"claude_mcp", FormatNameList(cfg.ClaudeMCP)})
	fields = append(fields, field{"builtin_plugins", FormatNameList(cfg.BuiltinPlugins)})
	// Message plane keys are always written explicitly (empty included) so
	// clearing the NATS URL persists, not just setting it.
	fields = append(fields,
		field{"msg_plane_mode", orDefaultStr(cfg.MsgPlaneMode, "pg")},
		field{"msg_plane_nats_url", cfg.MsgPlaneNATSURL},
		field{"msg_plane_subject", orDefaultStr(cfg.MsgPlaneSubject, "runs.dispatch")},
		field{"checkpoint_pg_host", cfg.CheckpointPGHost},
		field{"checkpoint_pg_port", orDefaultStr(cfg.CheckpointPGPort, "5432")},
		field{"checkpoint_pg_db", orDefaultStr(cfg.CheckpointPGDB, "woca")},
		field{"checkpoint_pg_user", cfg.CheckpointPGUser},
		field{"memory_pg_host", cfg.MemoryPGHost},
		field{"memory_pg_port", orDefaultStr(cfg.MemoryPGPort, "5432")},
		field{"memory_pg_db", orDefaultStr(cfg.MemoryPGDB, "woca")},
		field{"memory_pg_user", cfg.MemoryPGUser},
	)
	if cfg.MaxTokens != 0 {
		fields = append(fields, field{"max_tokens", strconv.Itoa(cfg.MaxTokens)})
	}
	if cfg.Temperature != 0 {
		fields = append(fields, field{"temperature", strconv.FormatFloat(cfg.Temperature, 'f', -1, 64)})
	}
	// thinking_budget is always written explicitly (0 = off included) so
	// turning thinking off persists, unlike empty-means-skip fields.
	fields = append(fields, field{"thinking_budget", strconv.Itoa(cfg.ThinkingBudget)})
	// debug_log is always written explicitly (false included) so turning the
	// observability log off persists, unlike empty-means-skip fields.
	fields = append(fields, field{"debug_log", strconv.FormatBool(cfg.DebugLog)})
	fields = append(fields, field{"ladym_autoretain", strconv.FormatBool(cfg.LadyMAutoRetain)})
	// Endpoint/username ride with the engine keys (the password is a
	// secret); always written explicitly so clearing them persists.
	fields = append(fields, field{"ladym_endpoint", cfg.LadyMEndpoint})
	fields = append(fields, field{"ladym_api_username", cfg.LadyMAPIUsername})
	fields = append(fields, field{"ladym_system2", strconv.FormatBool(cfg.LadyMSystem2)})
	if cfg.LadyMTopK != 0 {
		fields = append(fields, field{"ladym_topk", strconv.Itoa(cfg.LadyMTopK)})
	}
	if cfg.LadyMSystem2Interval != 0 {
		fields = append(fields, field{"ladym_system2_interval", strconv.Itoa(cfg.LadyMSystem2Interval)})
	}
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		if err := s.put("settings", "key", f.key, f.val); err != nil {
			return err
		}
	}
	return nil
}

// Clear removes a settings key so its default applies again.
func (s *Store) Clear(key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("settings: store disabled")
	}
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	if err != nil && s.db.Dialect == wocadb.DialectPostgres {
		_, err = s.db.Exec(`DELETE FROM settings WHERE key = $1`, key)
	}
	return err
}

// put upserts a row using the driver's placeholder style.
func (s *Store) put(table, keyCol, key, value string) error {
	var err error
	switch s.db.Dialect {
	case wocadb.DialectPostgres:
		_, err = s.db.Exec(
			fmt.Sprintf(`INSERT INTO %s (%s, value, updated_at) VALUES ($1, $2, $3)
				ON CONFLICT (%s) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`, table, keyCol, keyCol),
			key, value, wocadb.Now())
	default:
		_, err = s.db.Exec(
			fmt.Sprintf(`INSERT INTO %s (%s, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT(%s) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, table, keyCol, keyCol),
			key, value, wocadb.Now())
	}
	return err
}

// SetSecret encrypts value and stores it under name. An empty value removes
// the secret.
func (s *Store) SetSecret(name, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("settings: store disabled")
	}
	if value == "" {
		_, err := s.db.Exec(s.q(`DELETE FROM secrets WHERE name = ?`), name)
		return err
	}
	ct, err := s.crypto.Encrypt(value)
	if err != nil {
		return err
	}
	switch s.db.Dialect {
	case wocadb.DialectPostgres:
		_, err = s.db.Exec(`INSERT INTO secrets (name, ciphertext, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = EXCLUDED.updated_at`,
			name, ct, wocadb.Now())
	default:
		_, err = s.db.Exec(`INSERT INTO secrets (name, ciphertext, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
			name, ct, wocadb.Now())
	}
	return err
}

// GetSecret decrypts and returns the secret under name ("" when absent).
func (s *Store) GetSecret(name string) string {
	if s == nil || s.db == nil {
		return ""
	}
	var ct []byte
	err := s.db.QueryRow(s.q(`SELECT ciphertext FROM secrets WHERE name = ?`), name).Scan(&ct)
	if err != nil {
		return ""
	}
	v, err := s.crypto.Decrypt(ct)
	if err != nil {
		return ""
	}
	return v
}

// HasSecret reports whether a secret is stored under name.
func (s *Store) HasSecret(name string) bool {
	if s == nil || s.db == nil {
		return false
	}
	var one int
	return s.db.QueryRow(s.q(`SELECT 1 FROM secrets WHERE name = ?`), name).Scan(&one) == nil
}

// SecretNames lists stored secret names (values are never exposed).
func (s *Store) SecretNames() []string {
	if s == nil || s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT name FROM secrets ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// parseNameList decodes a persisted JSON string array; a malformed value
// yields nil rather than an error (settings load is best-effort).
func parseNameList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil
	}
	return out
}

// formatNameList encodes a name list for the settings table ("[]" for empty
// so an empty selection round-trips).
func FormatNameList(list []string) string {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// q rewrites the single placeholder style used by woca queries: statements
// are authored with ? and translated to $1… for PostgreSQL.
func (s *Store) q(query string) string {
	if s.db == nil || s.db.Dialect != wocadb.DialectPostgres {
		return query
	}
	var b []byte
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b = append(b, []byte(fmt.Sprintf("$%d", n))...)
		} else {
			b = append(b, query[i])
		}
	}
	return string(b)
}

// UpdatedAt returns the last modification time of a settings key (zero when
// absent) — used by the UI to show freshness.
func (s *Store) UpdatedAt(key string) time.Time {
	if s == nil || s.db == nil {
		return time.Time{}
	}
	var ms int64
	if err := s.db.QueryRow(s.q(`SELECT updated_at FROM settings WHERE key = ?`), key).Scan(&ms); err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// PostgresDSN assembles a postgres DSN from per-subsystem connection
// parts, URL-escaping the credential components (the console collects
// fields; the DSN is assembled here, never stored whole). Empty port/db
// fall back to 5432/woca.
func PostgresDSN(host, port, dbname, user, password string) string {
	if port == "" {
		port = "5432"
	}
	if dbname == "" {
		dbname = "woca"
	}
	u := "postgres://" + url.UserPassword(user, password).String() + "@" + host + ":" + port + "/" + dbname
	return u + "?sslmode=disable"
}

// CheckpointPostgresDSN assembles the checkpoint saver's DSN from the
// per-subsystem settings, falling back to the legacy postgres_url secret
// when no host is configured (older deployments).
func CheckpointPostgresDSN(cfg Settings, password string) string {
	if cfg.CheckpointPGHost == "" {
		return ""
	}
	return PostgresDSN(cfg.CheckpointPGHost, cfg.CheckpointPGPort, cfg.CheckpointPGDB, cfg.CheckpointPGUser, password)
}
