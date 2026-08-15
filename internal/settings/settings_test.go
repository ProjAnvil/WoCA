package settings

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

// openTest opens a Store backed by a temp sqlite database with the crypto key
// under a temp HOME.
func openTest(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, err := Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSettingsRoundTrip(t *testing.T) {
	st := openTest(t)
	cfg := Settings{
		Provider:          "openai",
		Model:             "gpt-4o",
		MaxTokens:         4096,
		Temperature:       0.5,
		AnthropicBaseURL:  "https://proxy.example",
		CheckpointBackend: CheckpointPostgres,
		MemoryBackend:     MemorySQLite,
		CheckpointDir:     "/tmp/cp",
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got := st.Load()
	if got.Provider != "openai" || got.Model != "gpt-4o" || got.MaxTokens != 4096 ||
		got.Temperature != 0.5 || got.AnthropicBaseURL != "https://proxy.example" ||
		got.CheckpointBackend != CheckpointPostgres || got.MemoryBackend != MemorySQLite ||
		got.CheckpointDir != "/tmp/cp" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestDefaultBackends(t *testing.T) {
	st := openTest(t)
	got := st.Load()
	if got.CheckpointBackend != CheckpointSQLite || got.MemoryBackend != MemorySQLite {
		t.Fatalf("defaults = %q/%q, want sqlite/sqlite", got.CheckpointBackend, got.MemoryBackend)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("default provider = %q", got.Provider)
	}
}

func TestSecretsEncryptedAtRest(t *testing.T) {
	st := openTest(t)
	if err := st.SetSecret(SecretAnthropicAPIKey, "sk-ant-plaintext"); err != nil {
		t.Fatal(err)
	}
	if v := st.GetSecret(SecretAnthropicAPIKey); v != "sk-ant-plaintext" {
		t.Fatalf("GetSecret = %q", v)
	}
	if !st.HasSecret(SecretAnthropicAPIKey) {
		t.Fatal("HasSecret = false after set")
	}
	// Raw row must not contain the plaintext.
	var ct []byte
	if err := st.DB().QueryRow(`SELECT ciphertext FROM secrets WHERE name = ?`, SecretAnthropicAPIKey).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if len(ct) == 0 || string(ct) == "sk-ant-plaintext" {
		t.Fatalf("ciphertext at rest leaks plaintext: %q", ct)
	}
	// Clearing removes it.
	if err := st.SetSecret(SecretAnthropicAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	if st.HasSecret(SecretAnthropicAPIKey) {
		t.Fatal("secret still present after clear")
	}
}

func TestApplyValidates(t *testing.T) {
	var cfg Settings
	if err := Apply(&cfg, KeyProvider, "bogus"); err == nil {
		t.Fatal("expected provider validation error")
	}
	if err := Apply(&cfg, KeyCheckpoint, "oracle"); err == nil {
		t.Fatal("expected checkpoint validation error")
	}
	if err := Apply(&cfg, KeyMemory, "sqlite"); err != nil {
		t.Fatalf("valid memory backend rejected: %v", err)
	}
	if err := Apply(&cfg, KeyMaxTokens, "-3"); err == nil {
		t.Fatal("expected max-tokens validation error")
	}
	if err := Apply(&cfg, KeyAnthropicAPIKey, "x"); err == nil {
		t.Fatal("secret key must route through SetSecret")
	}
	if _, ok := SecretFor(KeyOpenAIAPIKey); !ok {
		t.Fatal("openai-api-key not mapped to a secret")
	}
}

func TestNilStoreBehaves(t *testing.T) {
	var st *Store
	if v := st.GetSecret("x"); v != "" {
		t.Fatal("nil store leaked a secret")
	}
	if st.HasSecret("x") {
		t.Fatal("nil store claims a secret")
	}
	if d := st.Load(); d.CheckpointBackend != CheckpointSQLite {
		t.Fatalf("nil store defaults = %+v", d)
	}
}

func TestDialectPlaceholderRewrite(t *testing.T) {
	st := &Store{db: &wocadb.DB{DB: &sql.DB{}, Dialect: wocadb.DialectPostgres}}
	got := st.q(`SELECT * FROM t WHERE a = ? AND b = ?`)
	want := `SELECT * FROM t WHERE a = $1 AND b = $2`
	if got != want {
		t.Fatalf("q() = %q, want %q", got, want)
	}
}
