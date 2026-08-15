// Package settings persists woca's user-facing configuration (provider,
// model, endpoints, storage backends) in the woca database, with API keys and
// DSNs encrypted at rest via AES-256-GCM.
package settings

import (
	"fmt"
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

// Secret names stored encrypted in the secrets table.
const (
	SecretAnthropicAPIKey = "anthropic_api_key"
	SecretOpenAIAPIKey    = "openai_api_key"
	SecretPostgresURL     = "postgres_url"
)

// Settings is the persisted configuration. Secrets (API keys, the postgres
// DSN) are deliberately not part of this struct; they live in the secrets
// table and are accessed via Store secrets methods.
type Settings struct {
	Provider          string  `json:"provider"`
	Model             string  `json:"model"`
	MaxTokens         int     `json:"max_tokens"`
	Temperature       float64 `json:"temperature"`
	AnthropicBaseURL  string  `json:"anthropic_base_url"`
	OpenAIBaseURL     string  `json:"openai_base_url"`
	CheckpointBackend string  `json:"checkpoint_backend"`
	MemoryBackend     string  `json:"memory_backend"`
	CheckpointDir     string  `json:"checkpoint_dir"`
}

// Default returns the default settings: sqlite storage under ~/.woca.
func Default() Settings {
	return Settings{
		Provider:          "anthropic",
		CheckpointBackend: CheckpointSQLite,
		MemoryBackend:     MemorySQLite,
	}
}

// Normalize fills empty fields with defaults and validates backend names.
func (s Settings) Normalize() (Settings, error) {
	out := s
	if out.Provider == "" {
		out.Provider = "anthropic"
	}
	switch out.Provider {
	case "anthropic", "openai":
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
	return out, nil
}

// Store reads and writes settings plus encrypted secrets in a woca database.
type Store struct {
	db     *wocadb.DB
	crypto *wocadb.Crypto
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
		return n
	}
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
	if v, ok := pairs["anthropic_base_url"]; ok {
		out.AnthropicBaseURL = v
	}
	if v, ok := pairs["openai_base_url"]; ok {
		out.OpenAIBaseURL = v
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
}

// Save persists non-empty settings fields (empty fields are left unchanged so
// partial updates from the UI don't wipe prior values; use Clear to remove).
func (s *Store) Save(cfg Settings) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("settings: store disabled")
	}
	type field struct{ key, val string }
	fields := []field{
		{"provider", cfg.Provider},
		{"model", cfg.Model},
		{"anthropic_base_url", cfg.AnthropicBaseURL},
		{"openai_base_url", cfg.OpenAIBaseURL},
		{"checkpoint_backend", cfg.CheckpointBackend},
		{"memory_backend", cfg.MemoryBackend},
		{"checkpoint_dir", cfg.CheckpointDir},
	}
	if cfg.MaxTokens != 0 {
		fields = append(fields, field{"max_tokens", strconv.Itoa(cfg.MaxTokens)})
	}
	if cfg.Temperature != 0 {
		fields = append(fields, field{"temperature", strconv.FormatFloat(cfg.Temperature, 'f', -1, 64)})
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
