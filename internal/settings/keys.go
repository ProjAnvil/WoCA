package settings

import (
	"fmt"
	"strconv"
	"strings"
)

// Setting keys shared by `woca config`, the TUI /settings command, and the
// web settings API. Secret keys are stored encrypted in the secrets table.
const (
	KeyProvider              = "provider"
	KeyModel                 = "model"
	KeyMaxTokens             = "max-tokens"
	KeyTemperature           = "temperature"
	KeyAnthropicBaseURL      = "anthropic-base-url"
	KeyOpenAIBaseURL         = "openai-base-url"
	KeyAnthropicAPIKey       = "anthropic-api-key"
	KeyOpenAIAPIKey          = "openai-api-key"
	KeyCheckpoint            = "checkpoint"
	KeyMemory                = "memory"
	KeyCheckpointDir         = "checkpoint-dir"
	KeyPostgresURL           = "postgres-url"
	KeyMemoryEngine          = "memory-engine"
	KeyLadyMDBPath           = "ladym-db-path"
	KeyLadyMEmbedding        = "ladym-embedding"
	KeyLadyMEmbeddingModel   = "ladym-embedding-model"
	KeyLadyMEmbeddingBaseURL = "ladym-embedding-base-url"
	KeyLadyMEmbeddingAPIKey  = "ladym-embedding-api-key"
	KeyLadyMLLM              = "ladym-llm"
	KeyLadyMAutoRetain       = "ladym-autoretain"
	KeyLadyMTopK             = "ladym-topk"
	KeyLadyMSystem2          = "ladym-system2"
	KeyLadyMSystem2Interval  = "ladym-system2-interval"
	KeyDebugLog              = "debug-log"
)

// KeyNames lists all settable keys in display order.
func KeyNames() []string {
	return []string{
		KeyProvider, KeyModel, KeyMaxTokens, KeyTemperature,
		KeyAnthropicBaseURL, KeyOpenAIBaseURL, KeyAnthropicAPIKey, KeyOpenAIAPIKey,
		KeyCheckpoint, KeyMemory, KeyCheckpointDir, KeyPostgresURL,
		KeyMemoryEngine, KeyLadyMDBPath, KeyLadyMEmbedding, KeyLadyMEmbeddingModel,
		KeyLadyMEmbeddingBaseURL, KeyLadyMEmbeddingAPIKey, KeyLadyMLLM, KeyLadyMAutoRetain,
		KeyLadyMTopK, KeyLadyMSystem2, KeyLadyMSystem2Interval,
		KeyDebugLog,
	}
}

// SecretFor maps a setting key to its secret name; ok is false for
// non-secret keys.
func SecretFor(key string) (string, bool) {
	switch key {
	case KeyAnthropicAPIKey:
		return SecretAnthropicAPIKey, true
	case KeyOpenAIAPIKey:
		return SecretOpenAIAPIKey, true
	case KeyPostgresURL:
		return SecretPostgresURL, true
	case KeyLadyMEmbeddingAPIKey:
		return SecretLadyMEmbeddingAPIKey, true
	}
	return "", false
}

// KeyHelp returns a short usage line for a key.
func KeyHelp(key string) string {
	switch key {
	case KeyProvider:
		return "provider: anthropic | openai"
	case KeyModel:
		return "default model name (e.g. claude-sonnet-4-5, gpt-4o)"
	case KeyMaxTokens:
		return "max output tokens (> 0)"
	case KeyTemperature:
		return "sampling temperature (0-2)"
	case KeyAnthropicBaseURL:
		return "anthropic API base URL"
	case KeyOpenAIBaseURL:
		return "openai API base URL"
	case KeyAnthropicAPIKey:
		return "anthropic API key (stored encrypted)"
	case KeyOpenAIAPIKey:
		return "openai API key (stored encrypted)"
	case KeyCheckpoint:
		return "checkpoint backend: none | file | sqlite | postgres (default sqlite)"
	case KeyMemory:
		return "memoryStore backend: none | memory | sqlite | postgres (default sqlite)"
	case KeyCheckpointDir:
		return "directory for the file checkpoint backend"
	case KeyPostgresURL:
		return "postgres DSN for postgres backends (stored encrypted)"
	case KeyMemoryEngine:
		return "factual memory engine: ladym | none (default ladym)"
	case KeyLadyMDBPath:
		return "ladym sqlite db path (default ~/.woca/ladym.db)"
	case KeyLadyMEmbedding:
		return "embedding: inherit | hashing | openai | ollama | http (default inherit)"
	case KeyLadyMEmbeddingModel:
		return "embedding model (openai default text-embedding-3-small)"
	case KeyLadyMEmbeddingBaseURL:
		return "embedding base URL (inherit uses openai-base-url)"
	case KeyLadyMEmbeddingAPIKey:
		return "embedding API key (stored encrypted; empty inherits openai-api-key)"
	case KeyLadyMLLM:
		return "offline consolidation LLM: inherit | none (default inherit)"
	case KeyLadyMAutoRetain:
		return "auto-remember each run's Q/A and record an episode: true | false"
	case KeyLadyMTopK:
		return "recall top-k per model turn (1-20, default 6)"
	case KeyLadyMSystem2:
		return "background consolidation after runs (uses LLM): true | false"
	case KeyLadyMSystem2Interval:
		return "min seconds between System 2 cycles per workspace (>=60, default 300)"
	case KeyDebugLog:
		return "harness observability log (graph/state/session/message traces to ~/.woca/logs): true | false"
	}
	return "unknown key"
}

// Apply sets cfg's field for key to value, validating enums and numbers.
func Apply(cfg *Settings, key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case KeyProvider:
		switch value {
		case "anthropic", "openai":
		default:
			return fmt.Errorf("provider must be anthropic or openai, got %q", value)
		}
		cfg.Provider = value
	case KeyModel:
		cfg.Model = value
	case KeyMaxTokens:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("max-tokens must be a positive integer, got %q", value)
		}
		cfg.MaxTokens = n
	case KeyTemperature:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 2 {
			return fmt.Errorf("temperature must be a number in [0,2], got %q", value)
		}
		cfg.Temperature = f
	case KeyAnthropicBaseURL:
		cfg.AnthropicBaseURL = value
	case KeyOpenAIBaseURL:
		cfg.OpenAIBaseURL = value
	case KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyPostgresURL, KeyLadyMEmbeddingAPIKey:
		return fmt.Errorf("%s is a secret; use SetSecret", key)
	case KeyCheckpoint:
		switch value {
		case CheckpointNone, CheckpointFile, CheckpointSQLite, CheckpointPostgres:
		default:
			return fmt.Errorf("checkpoint must be none|file|sqlite|postgres, got %q", value)
		}
		cfg.CheckpointBackend = value
	case KeyMemory:
		switch value {
		case MemoryNone, MemoryInMem, MemorySQLite, MemoryPostgres:
		default:
			return fmt.Errorf("memory must be none|memory|sqlite|postgres, got %q", value)
		}
		cfg.MemoryBackend = value
	case KeyCheckpointDir:
		cfg.CheckpointDir = value
	case KeyMemoryEngine:
		switch value {
		case MemoryEngineLadyM, MemoryEngineNone:
		default:
			return fmt.Errorf("memory-engine must be ladym|none, got %q", value)
		}
		cfg.MemoryEngine = value
	case KeyLadyMDBPath:
		cfg.LadyMDBPath = value
	case KeyLadyMEmbedding:
		switch value {
		case LadyMEmbeddingInherit, LadyMEmbeddingHashing, LadyMEmbeddingOpenAI, LadyMEmbeddingOllama, LadyMEmbeddingHTTP:
		default:
			return fmt.Errorf("ladym-embedding must be inherit|hashing|openai|ollama|http, got %q", value)
		}
		cfg.LadyMEmbedding = value
	case KeyLadyMEmbeddingModel:
		cfg.LadyMEmbeddingModel = value
	case KeyLadyMEmbeddingBaseURL:
		cfg.LadyMEmbeddingBaseURL = value
	case KeyLadyMLLM:
		switch value {
		case LadyMLLMInherit, LadyMLLMNone:
		default:
			return fmt.Errorf("ladym-llm must be inherit|none, got %q", value)
		}
		cfg.LadyMLLM = value
	case KeyLadyMAutoRetain:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.LadyMAutoRetain = b
	case KeyLadyMTopK:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > MaxLadyMTopK {
			return fmt.Errorf("ladym-topk must be an integer in [1,%d], got %q", MaxLadyMTopK, value)
		}
		cfg.LadyMTopK = n
	case KeyLadyMSystem2:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.LadyMSystem2 = b
	case KeyLadyMSystem2Interval:
		n, err := strconv.Atoi(value)
		if err != nil || n < MinLadyMSystem2Interval {
			return fmt.Errorf("ladym-system2-interval must be an integer >= %d, got %q", MinLadyMSystem2Interval, value)
		}
		cfg.LadyMSystem2Interval = n
	case KeyDebugLog:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.DebugLog = b
	default:
		return fmt.Errorf("unknown settings key %q", key)
	}
	return nil
}

// Describe renders the settings for terminals: one line per key, secrets
// masked. secretSet reports whether a secret name is stored.
func Describe(cfg Settings, secretSet func(string) bool, dbPath string) string {
	mask := func(name string) string {
		if secretSet(name) {
			return "(set, encrypted)"
		}
		return "(not set)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "database: %s\n", dbPath)
	fmt.Fprintf(&b, "provider: %s\n", cfg.Provider)
	fmt.Fprintf(&b, "model: %s\n", orDefault(cfg.Model, "(provider default)"))
	fmt.Fprintf(&b, "max-tokens: %s\n", orDefault(strconv.Itoa(cfg.MaxTokens), "(default)"))
	fmt.Fprintf(&b, "temperature: %s\n", orDefault(strconv.FormatFloat(cfg.Temperature, 'f', -1, 64), "(default)"))
	fmt.Fprintf(&b, "anthropic-base-url: %s\n", orDefault(cfg.AnthropicBaseURL, "(default)"))
	fmt.Fprintf(&b, "openai-base-url: %s\n", orDefault(cfg.OpenAIBaseURL, "(default)"))
	fmt.Fprintf(&b, "anthropic-api-key: %s\n", mask(SecretAnthropicAPIKey))
	fmt.Fprintf(&b, "openai-api-key: %s\n", mask(SecretOpenAIAPIKey))
	fmt.Fprintf(&b, "checkpoint: %s\n", orDefault(cfg.CheckpointBackend, CheckpointSQLite))
	fmt.Fprintf(&b, "memory: %s\n", orDefault(cfg.MemoryBackend, MemorySQLite))
	fmt.Fprintf(&b, "checkpoint-dir: %s\n", orDefault(cfg.CheckpointDir, "(default)"))
	fmt.Fprintf(&b, "postgres-url: %s\n", mask(SecretPostgresURL))
	fmt.Fprintf(&b, "memory-engine: %s\n", orDefault(cfg.MemoryEngine, MemoryEngineLadyM))
	fmt.Fprintf(&b, "ladym-db-path: %s\n", orDefault(cfg.LadyMDBPath, "(default ~/.woca/ladym.db)"))
	fmt.Fprintf(&b, "ladym-embedding: %s\n", orDefault(cfg.LadyMEmbedding, LadyMEmbeddingInherit))
	fmt.Fprintf(&b, "ladym-embedding-model: %s\n", orDefault(cfg.LadyMEmbeddingModel, "(default)"))
	fmt.Fprintf(&b, "ladym-embedding-base-url: %s\n", orDefault(cfg.LadyMEmbeddingBaseURL, "(inherit)"))
	fmt.Fprintf(&b, "ladym-embedding-api-key: %s\n", mask(SecretLadyMEmbeddingAPIKey))
	fmt.Fprintf(&b, "ladym-llm: %s\n", orDefault(cfg.LadyMLLM, LadyMLLMInherit))
	fmt.Fprintf(&b, "ladym-autoretain: %s\n", strconv.FormatBool(cfg.LadyMAutoRetain))
	fmt.Fprintf(&b, "ladym-topk: %s\n", orDefault(strconv.Itoa(cfg.LadyMTopK), strconv.Itoa(DefaultLadyMTopK)))
	fmt.Fprintf(&b, "ladym-system2: %s\n", strconv.FormatBool(cfg.LadyMSystem2))
	fmt.Fprintf(&b, "ladym-system2-interval: %s\n", orDefault(strconv.Itoa(cfg.LadyMSystem2Interval), strconv.Itoa(DefaultLadyMSystem2Interval)))
	fmt.Fprintf(&b, "debug-log: %s\n", strconv.FormatBool(cfg.DebugLog))
	return b.String()
}

// KeyValue returns the raw value of a non-secret settings key ("" when the
// key is unknown or unset).
func KeyValue(cfg Settings, key string) string {
	switch key {
	case KeyProvider:
		return cfg.Provider
	case KeyModel:
		return cfg.Model
	case KeyMaxTokens:
		return strconv.Itoa(cfg.MaxTokens)
	case KeyTemperature:
		return strconv.FormatFloat(cfg.Temperature, 'f', -1, 64)
	case KeyAnthropicBaseURL:
		return cfg.AnthropicBaseURL
	case KeyOpenAIBaseURL:
		return cfg.OpenAIBaseURL
	case KeyCheckpoint:
		return cfg.CheckpointBackend
	case KeyMemory:
		return cfg.MemoryBackend
	case KeyCheckpointDir:
		return cfg.CheckpointDir
	case KeyMemoryEngine:
		return cfg.MemoryEngine
	case KeyLadyMDBPath:
		return cfg.LadyMDBPath
	case KeyLadyMEmbedding:
		return cfg.LadyMEmbedding
	case KeyLadyMEmbeddingModel:
		return cfg.LadyMEmbeddingModel
	case KeyLadyMEmbeddingBaseURL:
		return cfg.LadyMEmbeddingBaseURL
	case KeyLadyMLLM:
		return cfg.LadyMLLM
	case KeyLadyMAutoRetain:
		return strconv.FormatBool(cfg.LadyMAutoRetain)
	case KeyLadyMTopK:
		return strconv.Itoa(cfg.LadyMTopK)
	case KeyLadyMSystem2:
		return strconv.FormatBool(cfg.LadyMSystem2)
	case KeyLadyMSystem2Interval:
		return strconv.Itoa(cfg.LadyMSystem2Interval)
	case KeyDebugLog:
		return strconv.FormatBool(cfg.DebugLog)
	}
	return ""
}

// ParseBool accepts the spellings users actually type for boolean settings.
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes", "y":
		return true, nil
	case "false", "0", "off", "no", "n":
		return false, nil
	}
	return false, fmt.Errorf("expected true/false, got %q", v)
}

func orDefault(v, def string) string {
	if v == "" || v == "0" {
		return def
	}
	return v
}
