package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestApplyAllKeysAndErrors(t *testing.T) {
	valid := [][2]string{
		{KeyProvider, "glm"},
		{KeyModel, "gpt-4o"},
		{KeyMaxTokens, "4096"},
		{KeyTemperature, "0.5"},
		{KeyThinkingBudget, "2048"},
		{KeyAnthropicBaseURL, "http://a"},
		{KeyOpenAIBaseURL, "http://o"},
		{KeyGLMBaseURL, "http://g"},
		{KeyDeepSeekBaseURL, "http://d"},
		{KeyClaudeSkills, `["a","b"]`},
		{KeyClaudeMCP, `["m"]`},
		{KeyBuiltinPlugins, `["browser"]`},
		{KeyCheckpoint, "file"},
		{KeyMemory, "postgres"},
		{KeyCheckpointDir, "/tmp/ck"},
		{KeyMemoryEngine, "none"},
		{KeyLadyMDBPath, "/tmp/ladym.db"},
		{KeyLadyMEmbedding, "openai"},
		{KeyLadyMEmbeddingModel, "text-embedding-3-small"},
		{KeyLadyMEmbeddingBaseURL, "http://e"},
		{KeyLadyMLLM, "none"},
		{KeyLadyMAutoRetain, "true"},
		{KeyLadyMTopK, "12"},
		{KeyLadyMSystem2, "false"},
		{KeyLadyMSystem2Interval, "120"},
		{KeyDebugLog, "true"},
	}
	cfg := Settings{}
	for _, kv := range valid {
		if err := Apply(&cfg, kv[0], kv[1]); err != nil {
			t.Fatalf("Apply(%s) = %v", kv[0], err)
		}
	}
	if cfg.Provider != "glm" || cfg.Model != "gpt-4o" || cfg.MaxTokens != 4096 || cfg.Temperature != 0.5 {
		t.Fatalf("cfg scalars = %+v", cfg)
	}
	if cfg.ThinkingBudget != 2048 || cfg.CheckpointBackend != "file" || cfg.MemoryBackend != "postgres" {
		t.Fatalf("cfg backends = %+v", cfg)
	}
	if cfg.MemoryEngine != "none" || cfg.LadyMEmbedding != "openai" || cfg.LadyMLLM != "none" {
		t.Fatalf("cfg memory = %+v", cfg)
	}
	if !cfg.LadyMAutoRetain || cfg.LadyMSystem2 || !cfg.DebugLog {
		t.Fatalf("cfg bools = %+v", cfg)
	}
	if cfg.LadyMTopK != 12 || cfg.LadyMSystem2Interval != 120 {
		t.Fatalf("cfg ladym = %+v", cfg)
	}
	if len(cfg.ClaudeSkills) != 2 || len(cfg.ClaudeMCP) != 1 || len(cfg.BuiltinPlugins) != 1 {
		t.Fatalf("cfg lists = %+v", cfg)
	}

	invalid := [][2]string{
		{KeyProvider, "nope"},
		{KeyMaxTokens, "-3"},
		{KeyMaxTokens, "abc"},
		{KeyTemperature, "3"},
		{KeyTemperature, "x"},
		{KeyThinkingBudget, "-1"},
		{KeyThinkingBudget, "99999"},
		{KeyAnthropicAPIKey, "secret"},
		{KeyOpenAIAPIKey, "secret"},
		{KeyGLMAPIKey, "secret"},
		{KeyDeepSeekAPIKey, "secret"},
		{KeyPostgresURL, "postgres://x"},
		{KeyLadyMEmbeddingAPIKey, "secret"},
		{KeyClaudeSkills, "not json"},
		{KeyClaudeMCP, "not json"},
		{KeyBuiltinPlugins, "not json"},
		{KeyCheckpoint, "bamboo"},
		{KeyMemory, "bamboo"},
		{KeyMemoryEngine, "bamboo"},
		{KeyLadyMEmbedding, "bamboo"},
		{KeyLadyMLLM, "bamboo"},
		{KeyLadyMAutoRetain, "maybe"},
		{KeyLadyMTopK, "0"},
		{KeyLadyMTopK, "99"},
		{KeyLadyMSystem2, "maybe"},
		{KeyLadyMSystem2Interval, "10"},
		{KeyDebugLog, "maybe"},
		{"no-such-key", "x"},
	}
	for _, kv := range invalid {
		if err := Apply(&Settings{}, kv[0], kv[1]); err == nil {
			t.Errorf("Apply(%s, %q) should fail", kv[0], kv[1])
		}
	}
}

func TestKeyHelpAndNames(t *testing.T) {
	names := KeyNames()
	if len(names) == 0 {
		t.Fatal("no key names")
	}
	for _, k := range names {
		h := KeyHelp(k)
		if h == "" || h == "unknown key" {
			t.Errorf("KeyHelp(%s) = %q", k, h)
		}
	}
	if KeyHelp("nope") != "unknown key" {
		t.Error("unknown key help")
	}
}

func TestSecretFor(t *testing.T) {
	for _, k := range []string{KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyGLMAPIKey, KeyDeepSeekAPIKey, KeyPostgresURL, KeyLadyMEmbeddingAPIKey} {
		if _, ok := SecretFor(k); !ok {
			t.Errorf("%s should be a secret", k)
		}
	}
	if _, ok := SecretFor(KeyModel); ok {
		t.Error("model should not be a secret")
	}
}

func TestKeyValueAllKeys(t *testing.T) {
	cfg := Settings{
		Provider: "openai", Model: "m", MaxTokens: 5, Temperature: 1.5, ThinkingBudget: 7,
		AnthropicBaseURL: "a", OpenAIBaseURL: "o", GLMBaseURL: "g", DeepSeekBaseURL: "d",
		ClaudeSkills: []string{"s"}, ClaudeMCP: []string{"m"}, BuiltinPlugins: []string{"b"},
		CheckpointBackend: "file", MemoryBackend: "memory", CheckpointDir: "cd",
		MemoryEngine: "none", LadyMDBPath: "lp", LadyMEmbedding: "openai",
		LadyMEmbeddingModel: "em", LadyMEmbeddingBaseURL: "eb", LadyMLLM: "none",
		LadyMAutoRetain: true, LadyMTopK: 3, LadyMSystem2: true, LadyMSystem2Interval: 61,
		DebugLog: true,
	}
	for _, k := range KeyNames() {
		if _, ok := SecretFor(k); ok {
			continue
		}
		if v := KeyValue(cfg, k); v == "" {
			t.Errorf("KeyValue(%s) empty for %+v", k, cfg)
		}
	}
	if KeyValue(cfg, "nope") != "" {
		t.Error("unknown key should be empty")
	}
}

func TestDescribeAndParseHelpers(t *testing.T) {
	cfg := Default()
	out := Describe(cfg, func(name string) bool { return name == SecretOpenAIAPIKey }, "test.db")
	for _, want := range []string{"database: test.db", "provider: anthropic", "memory-engine: ladym"} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe missing %q", want)
		}
	}
	if !strings.Contains(out, "openai-api-key: (set, encrypted)") || !strings.Contains(out, "anthropic-api-key: (not set)") {
		t.Error("secret masking wrong")
	}

	list, err := ParseNameList(`["x"]`)
	if err != nil || len(list) != 1 {
		t.Fatalf("ParseNameList = %v %v", list, err)
	}
	if _, err := ParseNameList("nope"); err == nil {
		t.Error("expected parse error")
	}

	for _, c := range []struct {
		v    string
		want bool
	}{
		{"true", true}, {"1", true}, {"on", true}, {"YES", true}, {"Y", true},
		{"false", false}, {"0", false}, {"off", false}, {"No", false}, {"n", false},
	} {
		got, err := ParseBool(c.v)
		if err != nil || got != c.want {
			t.Errorf("ParseBool(%q) = %v %v", c.v, got, err)
		}
	}
	if _, err := ParseBool("maybe"); err == nil {
		t.Error("expected bool error")
	}

	if FormatNameList(nil) != "[]" || FormatNameList([]string{"a"}) != `["a"]` {
		t.Error("FormatNameList")
	}
	if parseNameList("bad") != nil || parseNameList("") != nil || len(parseNameList(`["a"]`)) != 1 {
		t.Error("parseNameList")
	}
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

func TestManagedOverlayAllFieldsAndLocks(t *testing.T) {
	st := newCryptoStore(t)
	base := Settings{
		Provider: "openai", Model: "m1", MaxTokens: 111, Temperature: 0.4,
		AnthropicBaseURL: "http://a", OpenAIBaseURL: "http://o", GLMBaseURL: "http://g", DeepSeekBaseURL: "http://d",
		CheckpointBackend: CheckpointPostgres, MemoryBackend: MemoryPostgres, CheckpointDir: "/ck",
		OutputStyle: "concise", MemoryEngine: MemoryEngineNone, LadyMDBPath: "/lady.db",
		LadyMEmbedding: LadyMEmbeddingOpenAI, LadyMEmbeddingModel: "emb-1", LadyMEmbeddingBaseURL: "http://emb",
		LadyMLLM: LadyMLLMNone, LadyMTopK: 9, LadyMSystem2Interval: 90,
	}
	st.SetManaged(base)
	got := st.Load()
	if got.Provider != "openai" || got.Model != "m1" || got.MaxTokens != 111 || got.Temperature != 0.4 {
		t.Fatalf("overlay scalars = %+v", got)
	}
	if got.CheckpointBackend != CheckpointPostgres || got.MemoryBackend != MemoryPostgres || got.CheckpointDir != "/ck" {
		t.Fatalf("overlay backends = %+v", got)
	}
	if got.OutputStyle != "concise" || got.MemoryEngine != MemoryEngineNone || got.LadyMDBPath != "/lady.db" {
		t.Fatalf("overlay misc = %+v", got)
	}
	if got.LadyMEmbedding != LadyMEmbeddingOpenAI || got.LadyMEmbeddingModel != "emb-1" || got.LadyMEmbeddingBaseURL != "http://emb" {
		t.Fatalf("overlay embedding = %+v", got)
	}
	if got.LadyMLLM != LadyMLLMNone || got.LadyMTopK != 9 || got.LadyMSystem2Interval != 90 {
		t.Fatalf("overlay ladym = %+v", got)
	}

	// Writes that would change any managed field are rejected.
	changed := got
	changed.Provider = "glm"
	if err := st.Save(changed); err == nil {
		t.Fatal("provider should be locked")
	}
	changed = got
	changed.Model = "other"
	if err := st.Save(changed); err == nil {
		t.Fatal("model should be locked")
	}
	changed = got
	changed.MaxTokens = 222
	if err := st.Save(changed); err == nil {
		t.Fatal("max-tokens should be locked")
	}
	changed = got
	changed.Temperature = 1
	if err := st.Save(changed); err == nil {
		t.Fatal("temperature should be locked")
	}
	for _, mut := range []func(*Settings){
		func(s *Settings) { s.AnthropicBaseURL = "x" },
		func(s *Settings) { s.OpenAIBaseURL = "x" },
		func(s *Settings) { s.CheckpointBackend = CheckpointFile },
		func(s *Settings) { s.MemoryBackend = MemorySQLite },
		func(s *Settings) { s.CheckpointDir = "x" },
		func(s *Settings) { s.OutputStyle = "x" },
		func(s *Settings) { s.MemoryEngine = MemoryEngineLadyM },
		func(s *Settings) { s.LadyMDBPath = "x" },
		func(s *Settings) { s.LadyMEmbedding = LadyMEmbeddingHashing },
		func(s *Settings) { s.LadyMEmbeddingModel = "x" },
		func(s *Settings) { s.LadyMEmbeddingBaseURL = "x" },
		func(s *Settings) { s.LadyMLLM = LadyMLLMInherit },
		func(s *Settings) { s.LadyMTopK = 5 },
		func(s *Settings) { s.LadyMSystem2Interval = 100 },
	} {
		changed := got
		mut(&changed)
		if err := st.Save(changed); err == nil {
			t.Errorf("field should be locked after mutation %+v", changed)
		}
	}
	// Saving the managed values themselves is fine, and clearing the overlay
	// restores the database values.
	if err := st.Save(got); err != nil {
		t.Fatalf("saving managed values = %v", err)
	}
	st.SetManaged(Settings{}) // empty overlay clears it
	// The managed values were persisted above, so they now come from the DB.
	if got := st.Load(); got.Model != "m1" || got.Provider != "openai" {
		t.Fatalf("after clear = %+v", got)
	}
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
