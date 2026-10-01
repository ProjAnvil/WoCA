package settings

import (
	"strings"
	"testing"
)

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
		LadyMAutoRetain: true, LadyMTopK: 3, LadyMSystem2: true, LadyMSystem2Interval: 61, LadyMEndpoint: "http://ladym:8080", LadyMAPIUsername: "woca-worker",
		DebugLog:     true,
		MsgPlaneMode: "pg+nats", MsgPlaneNATSURL: "nats://n:4222", MsgPlaneSubject: "runs.dispatch",
		CheckpointPGHost: "ck", CheckpointPGPort: "5433", CheckpointPGDB: "ckdb", CheckpointPGUser: "cku",
		MemoryPGHost: "mm", MemoryPGPort: "5434", MemoryPGDB: "mmdb", MemoryPGUser: "mmu",
		LangfuseEnabled: true, LangfuseBaseURL: "https://lf.example", LangfusePublicKey: "pk-lf",
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
