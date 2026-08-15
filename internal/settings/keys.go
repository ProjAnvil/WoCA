package settings

import (
	"fmt"
	"strconv"
	"strings"
)

// Setting keys shared by `woca config`, the TUI /settings command, and the
// web settings API. Secret keys are stored encrypted in the secrets table.
const (
	KeyProvider         = "provider"
	KeyModel            = "model"
	KeyMaxTokens        = "max-tokens"
	KeyTemperature      = "temperature"
	KeyAnthropicBaseURL = "anthropic-base-url"
	KeyOpenAIBaseURL    = "openai-base-url"
	KeyAnthropicAPIKey  = "anthropic-api-key"
	KeyOpenAIAPIKey     = "openai-api-key"
	KeyCheckpoint       = "checkpoint"
	KeyMemory           = "memory"
	KeyCheckpointDir    = "checkpoint-dir"
	KeyPostgresURL      = "postgres-url"
)

// KeyNames lists all settable keys in display order.
func KeyNames() []string {
	return []string{
		KeyProvider, KeyModel, KeyMaxTokens, KeyTemperature,
		KeyAnthropicBaseURL, KeyOpenAIBaseURL, KeyAnthropicAPIKey, KeyOpenAIAPIKey,
		KeyCheckpoint, KeyMemory, KeyCheckpointDir, KeyPostgresURL,
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
	case KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyPostgresURL:
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
	}
	return ""
}

func orDefault(v, def string) string {
	if v == "" || v == "0" {
		return def
	}
	return v
}
