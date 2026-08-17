package settings

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func newCryptoStore(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, err := Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestNormalizeDefaultsAndErrors(t *testing.T) {
	out, err := Settings{}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "anthropic" || out.CheckpointBackend != CheckpointNone || out.MemoryBackend != MemoryNone {
		t.Fatalf("defaults = %+v", out)
	}
	if out.MemoryEngine != MemoryEngineLadyM || out.LadyMEmbedding != LadyMEmbeddingInherit || out.LadyMLLM != LadyMLLMInherit {
		t.Fatalf("ladym defaults = %+v", out)
	}
	if out.LadyMTopK != DefaultLadyMTopK || out.LadyMSystem2Interval != DefaultLadyMSystem2Interval {
		t.Fatalf("numeric defaults = %+v", out)
	}

	for _, s := range []Settings{
		{Provider: "nope"},
		{CheckpointBackend: "nope"},
		{MemoryBackend: "nope"},
		{MemoryEngine: "nope"},
		{LadyMEmbedding: "nope"},
		{LadyMLLM: "nope"},
	} {
		if _, err := s.Normalize(); err == nil {
			t.Errorf("Normalize(%+v) should fail", s)
		}
	}
}

func TestStoreRoundTripAndSecrets(t *testing.T) {
	st := newCryptoStore(t)

	// Defaults from an empty DB.
	if got := st.Load(); got.Provider != "anthropic" || got.MemoryEngine != MemoryEngineLadyM {
		t.Fatalf("defaults = %+v", got)
	}

	cfg := st.Load()
	for _, kv := range [][2]string{
		{KeyProvider, "openai"},
		{KeyModel, "gpt-4o"},
		{KeyClaudeSkills, `["a"]`},
		{KeyBuiltinPlugins, `["pdf"]`},
		{KeyDebugLog, "true"},
	} {
		if err := Apply(&cfg, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got := st.Load()
	if got.Provider != "openai" || got.Model != "gpt-4o" || got.DebugLog != true {
		t.Fatalf("round trip = %+v", got)
	}
	if len(got.ClaudeSkills) != 1 || len(got.BuiltinPlugins) != 1 {
		t.Fatalf("lists = %+v", got)
	}

	// Secrets: set, read, list, clear.
	if err := st.SetSecret(SecretOpenAIAPIKey, "sk-abc"); err != nil {
		t.Fatal(err)
	}
	if v := st.GetSecret(SecretOpenAIAPIKey); v != "sk-abc" {
		t.Fatalf("secret = %q", v)
	}
	if !st.HasSecret(SecretOpenAIAPIKey) {
		t.Fatal("HasSecret false")
	}
	names := st.SecretNames()
	if len(names) != 1 || names[0] != SecretOpenAIAPIKey {
		t.Fatalf("names = %v", names)
	}
	// An empty value removes the secret.
	if err := st.SetSecret(SecretOpenAIAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	if st.HasSecret(SecretOpenAIAPIKey) {
		t.Fatal("secret should be removed")
	}
	// A corrupt ciphertext degrades to "".
	if err := st.SetSecret("corrupt", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE secrets SET ciphertext = X'00112233' WHERE name = 'corrupt'`, nil); err != nil {
		t.Fatal(err)
	}
	if v := st.GetSecret("corrupt"); v != "" {
		t.Fatalf("corrupt secret = %q", v)
	}

	// Clear removes a key; UpdatedAt reports the write time.
	if err := st.Clear(KeyModel); err != nil {
		t.Fatal(err)
	}
	if got := st.Load().Model; got != "" {
		t.Fatalf("after clear model = %q", got)
	}
	if err := Apply(&cfg, KeyModel, "fresh"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if ts := st.UpdatedAt(KeyModel); ts.IsZero() || time.Since(ts) > time.Minute {
		t.Fatalf("UpdatedAt = %v", ts)
	}
	if !st.UpdatedAt("nope-key").IsZero() {
		t.Fatal("absent UpdatedAt should be zero")
	}

	// Path/DB/Close.
	if !strings.Contains(st.Path(), "sqlite:") {
		t.Fatalf("Path = %q", st.Path())
	}
	if st.DB() == nil {
		t.Fatal("DB nil")
	}
}

func TestStoreDisabledAndOpenErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Disabled targets produce a nil store.
	if st, err := Open("none"); err != nil || st != nil {
		t.Fatalf("none = %v %v", st, err)
	}
	if st, err := Open("off"); err != nil || st != nil {
		t.Fatalf("off = %v %v", st, err)
	}

	// A database under a regular file fails to open.
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "woca.db")); err == nil {
		t.Fatal("expected open error")
	}

	// Nil-store behavior: everything degrades safely.
	var nilStore *Store
	if err := nilStore.Save(Settings{}); err == nil {
		t.Error("Save should fail")
	}
	if err := nilStore.Clear("k"); err == nil {
		t.Error("Clear should fail")
	}
	if err := nilStore.SetSecret("n", "v"); err == nil {
		t.Error("SetSecret should fail")
	}
	if nilStore.GetSecret("n") != "" || nilStore.HasSecret("n") || nilStore.SecretNames() != nil {
		t.Error("secret reads on nil store")
	}
	if nilStore.DB() != nil || nilStore.Path() != "disabled" {
		t.Error("nil DB/Path")
	}
	if !nilStore.UpdatedAt("k").IsZero() {
		t.Error("nil UpdatedAt")
	}
	if err := nilStore.Close(); err != nil {
		t.Error("nil Close")
	}
	if nilStore.Load().Provider != "anthropic" {
		t.Error("nil Load should give defaults")
	}
	nilStore.SetManaged(Settings{Provider: "x"}) // no panic
}

func TestOpenCryptoFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The DB opens fine in a temp dir, but the crypto key path under the
	// blocked home fails.
	t.Setenv("HOME", blocker)
	dbPath := filepath.Join(t.TempDir(), "woca.db")
	if _, err := Open(dbPath); err == nil {
		t.Fatal("expected crypto key failure")
	}
}

func TestStoreClosedDBDegradation(t *testing.T) {
	st := newCryptoStore(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Reads on a closed database fall back to defaults instead of crashing.
	if got := st.Load(); got.Provider != "anthropic" {
		t.Fatalf("closed Load = %+v", got)
	}
	if err := st.Save(Default()); err == nil {
		t.Fatal("closed Save should fail")
	}
	if err := st.Clear("k"); err == nil {
		t.Fatal("closed Clear should fail")
	}
	if err := st.SetSecret("n", "v"); err == nil {
		t.Fatal("closed SetSecret should fail")
	}
	_ = st.GetSecret("n")
	_ = st.HasSecret("n")
	_ = st.SecretNames()
	_ = st.UpdatedAt("k")
}
