package settings

import (
	"encoding/json"
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
	KeyThinkingBudget        = "thinking-budget"
	KeyAnthropicBaseURL      = "anthropic-base-url"
	KeyOpenAIBaseURL         = "openai-base-url"
	KeyGLMBaseURL            = "glm-base-url"
	KeyDeepSeekBaseURL       = "deepseek-base-url"
	KeyAnthropicAPIKey       = "anthropic-api-key"
	KeyOpenAIAPIKey          = "openai-api-key"
	KeyGLMAPIKey             = "glm-api-key"
	KeyDeepSeekAPIKey        = "deepseek-api-key"
	KeyClaudeSkills          = "claude-skills"
	KeyClaudeMCP             = "claude-mcp"
	KeyBuiltinPlugins        = "builtin-plugins"
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
	KeyLadyMEndpoint         = "ladym-endpoint"
	KeyLadyMAPIUsername      = "ladym-api-username"
	KeyLadyMAPIPassword      = "ladym-api-password"
	KeyDebugLog              = "debug-log"
	KeyMsgPlaneMode          = "msg-plane-mode"
	KeyCheckpointPGHost      = "checkpoint-pg-host"
	KeyCheckpointPGPort      = "checkpoint-pg-port"
	KeyCheckpointPGDB        = "checkpoint-pg-db"
	KeyCheckpointPGUser      = "checkpoint-pg-user"
	KeyMemoryPGHost          = "memory-pg-host"
	KeyMemoryPGPort          = "memory-pg-port"
	KeyMemoryPGDB            = "memory-pg-db"
	KeyMemoryPGUser          = "memory-pg-user"
	KeyMsgPlaneNATSURL       = "msg-plane-nats-url"
	KeyMsgPlaneSubject       = "msg-plane-subject"
)

// KeyNames lists all settable keys in display order.
func KeyNames() []string {
	return []string{
		KeyProvider, KeyModel, KeyMaxTokens, KeyTemperature, KeyThinkingBudget,
		KeyAnthropicBaseURL, KeyOpenAIBaseURL, KeyGLMBaseURL, KeyDeepSeekBaseURL,
		KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyGLMAPIKey, KeyDeepSeekAPIKey,
		KeyClaudeSkills, KeyClaudeMCP, KeyBuiltinPlugins,
		KeyCheckpoint, KeyMemory, KeyCheckpointDir, KeyPostgresURL,
		KeyMemoryEngine, KeyLadyMDBPath, KeyLadyMEmbedding, KeyLadyMEmbeddingModel,
		KeyLadyMEmbeddingBaseURL, KeyLadyMEmbeddingAPIKey, KeyLadyMLLM, KeyLadyMAutoRetain,
		KeyLadyMTopK, KeyLadyMSystem2, KeyLadyMSystem2Interval,
		KeyLadyMEndpoint, KeyLadyMAPIUsername, KeyLadyMAPIPassword,
		KeyDebugLog,
		KeyMsgPlaneMode, KeyMsgPlaneNATSURL, KeyMsgPlaneSubject,
		KeyCheckpointPGHost, KeyCheckpointPGPort, KeyCheckpointPGDB, KeyCheckpointPGUser,
		KeyMemoryPGHost, KeyMemoryPGPort, KeyMemoryPGDB, KeyMemoryPGUser,
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
	case KeyGLMAPIKey:
		return SecretGLMAPIKey, true
	case KeyDeepSeekAPIKey:
		return SecretDeepSeekAPIKey, true
	case KeyPostgresURL:
		return SecretPostgresURL, true
	case KeyLadyMEmbeddingAPIKey:
		return SecretLadyMEmbeddingAPIKey, true
	case KeyLadyMAPIPassword:
		return SecretLadyMAPIPassword, true
	case "checkpoint-pg-password":
		return SecretCheckpointPGPassword, true
	case "memory-pg-password":
		return SecretMemoryPGPassword, true
	}
	return "", false
}

// KeyHelp returns a short usage line for a key.
func KeyHelp(key string) string {
	switch key {
	case KeyProvider:
		return "provider: anthropic | openai | glm | deepseek"
	case KeyModel:
		return "default model name (e.g. claude-sonnet-4-5, gpt-4o, glm-4.6, deepseek-chat)"
	case KeyMaxTokens:
		return "max output tokens (> 0)"
	case KeyTemperature:
		return "sampling temperature (0-2)"
	case KeyThinkingBudget:
		return "extended-thinking budget in tokens (0-31999; 0 = off; provider-mapped)"
	case KeyAnthropicBaseURL:
		return "anthropic API base URL"
	case KeyOpenAIBaseURL:
		return "openai API base URL"
	case KeyGLMBaseURL:
		return "glm API base URL (default https://open.bigmodel.cn/api/anthropic)"
	case KeyDeepSeekBaseURL:
		return "deepseek API base URL (default https://api.deepseek.com/anthropic)"
	case KeyAnthropicAPIKey:
		return "anthropic API key (stored encrypted)"
	case KeyOpenAIAPIKey:
		return "openai API key (stored encrypted)"
	case KeyGLMAPIKey:
		return "glm API key (stored encrypted)"
	case KeyDeepSeekAPIKey:
		return "deepseek API key (stored encrypted)"
	case KeyClaudeSkills:
		return "enabled ~/.claude skill names as a JSON array"
	case KeyClaudeMCP:
		return "enabled ~/.claude.json MCP server names as a JSON array (connected at startup)"
	case KeyBuiltinPlugins:
		return "enabled built-in tool plugins as a JSON array (browser, pdf; runtime installed from Settings → 插件)"
	case KeyCheckpoint:
		return "checkpoint backend: none | file | sqlite | postgres (default sqlite; file keeps a sqlite checkpoints.db inside checkpoint-dir)"
	case KeyMemory:
		return "memoryStore backend: none | memory | sqlite | postgres (default sqlite)"
	case KeyCheckpointDir:
		return "directory for the file checkpoint backend"
	case KeyPostgresURL:
		return "postgres DSN for postgres backends (stored encrypted)"
	case KeyMemoryEngine:
		return "factual memory engine: ladym | remote | none (default ladym personal, remote for enterprise deployments)"
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
	case KeyLadyMEndpoint:
		return "ladyM enterprise API endpoint (scheme://host:port); required when memory-engine is remote"
	case KeyLadyMAPIUsername:
		return "ladyM enterprise API username (Basic auth users table)"
	case KeyLadyMAPIPassword:
		return "ladyM enterprise API password (stored encrypted)"
	case KeyDebugLog:
		return "harness observability log (graph/state/session/message traces to ~/.woca/logs): true | false"
	case KeyMsgPlaneMode:
		return "run dispatch message plane: pg | pg+nats (default pg; pg+nats adds NATS dispatch notifications on top of the PG queue)"
	case KeyMsgPlaneNATSURL:
		return "NATS server URL for pg+nats mode (nats://host:port)"
	case KeyMsgPlaneSubject:
		return "dispatch notification subject (default runs.dispatch)"
	case KeyCheckpointPGHost:
		return "checkpoint postgres host (shown when checkpoint backend is postgres; password goes in the checkpoint-pg-password secret)"
	case KeyCheckpointPGPort:
		return "checkpoint postgres port (default 5432)"
	case KeyCheckpointPGDB:
		return "checkpoint postgres database (default woca)"
	case KeyCheckpointPGUser:
		return "checkpoint postgres user"
	case KeyMemoryPGHost:
		return "memoryStore postgres host (shown when memory backend is postgres; password goes in the memory-pg-password secret)"
	case KeyMemoryPGPort:
		return "memoryStore postgres port (default 5432)"
	case KeyMemoryPGDB:
		return "memoryStore postgres database (default woca)"
	case KeyMemoryPGUser:
		return "memoryStore postgres user"
	}
	return "unknown key"
}

// Apply sets cfg's field for key to value, validating enums and numbers.
func Apply(cfg *Settings, key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case KeyProvider:
		switch value {
		case "anthropic", "openai", "glm", "deepseek":
		default:
			return fmt.Errorf("provider must be anthropic, openai, glm, or deepseek, got %q", value)
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
	case KeyThinkingBudget:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > MaxThinkingBudget {
			return fmt.Errorf("thinking-budget must be an integer in [0,%d], got %q", MaxThinkingBudget, value)
		}
		cfg.ThinkingBudget = n
	case KeyAnthropicBaseURL:
		cfg.AnthropicBaseURL = value
	case KeyOpenAIBaseURL:
		cfg.OpenAIBaseURL = value
	case KeyGLMBaseURL:
		cfg.GLMBaseURL = value
	case KeyDeepSeekBaseURL:
		cfg.DeepSeekBaseURL = value
	case KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyGLMAPIKey, KeyDeepSeekAPIKey, KeyPostgresURL, KeyLadyMEmbeddingAPIKey:
		return fmt.Errorf("%s is a secret; use SetSecret", key)
	case KeyClaudeSkills:
		list, err := ParseNameList(value)
		if err != nil {
			return err
		}
		cfg.ClaudeSkills = list
	case KeyClaudeMCP:
		list, err := ParseNameList(value)
		if err != nil {
			return err
		}
		cfg.ClaudeMCP = list
	case KeyBuiltinPlugins:
		list, err := ParseNameList(value)
		if err != nil {
			return err
		}
		cfg.BuiltinPlugins = list
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
		case MemoryEngineLadyM, MemoryEngineNone, MemoryEngineRemote:
		default:
			return fmt.Errorf("memory-engine must be ladym|none|remote, got %q", value)
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
	case KeyLadyMEndpoint:
		if value != "" && !strings.Contains(value, "://") {
			return fmt.Errorf("ladym-endpoint must be scheme://host:port, got %q", value)
		}
		cfg.LadyMEndpoint = value
	case KeyLadyMAPIUsername:
		cfg.LadyMAPIUsername = value
	case KeyDebugLog:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.DebugLog = b
	case KeyMsgPlaneMode:
		switch value {
		case "", "pg", "pg+nats":
		default:
			return fmt.Errorf("msg-plane-mode must be pg or pg+nats, got %q", value)
		}
		cfg.MsgPlaneMode = value
	case KeyMsgPlaneNATSURL:
		if value != "" && !strings.Contains(value, "://") {
			return fmt.Errorf("msg-plane-nats-url must be nats://host:port, got %q", value)
		}
		cfg.MsgPlaneNATSURL = value
	case KeyMsgPlaneSubject:
		cfg.MsgPlaneSubject = value
	case KeyCheckpointPGHost:
		cfg.CheckpointPGHost = value
	case KeyCheckpointPGPort:
		if value != "" {
			if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("checkpoint-pg-port must be an integer in [1,65535], got %q", value)
			}
		}
		cfg.CheckpointPGPort = value
	case KeyCheckpointPGDB:
		cfg.CheckpointPGDB = value
	case KeyCheckpointPGUser:
		cfg.CheckpointPGUser = value
	case KeyMemoryPGHost:
		cfg.MemoryPGHost = value
	case KeyMemoryPGPort:
		if value != "" {
			if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("memory-pg-port must be an integer in [1,65535], got %q", value)
			}
		}
		cfg.MemoryPGPort = value
	case KeyMemoryPGDB:
		cfg.MemoryPGDB = value
	case KeyMemoryPGUser:
		cfg.MemoryPGUser = value
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
	fmt.Fprintf(&b, "thinking-budget: %s\n", orDefault(strconv.Itoa(cfg.ThinkingBudget), "0 (off)"))
	fmt.Fprintf(&b, "anthropic-base-url: %s\n", orDefault(cfg.AnthropicBaseURL, "(default)"))
	fmt.Fprintf(&b, "openai-base-url: %s\n", orDefault(cfg.OpenAIBaseURL, "(default)"))
	fmt.Fprintf(&b, "glm-base-url: %s\n", orDefault(cfg.GLMBaseURL, "(default https://open.bigmodel.cn/api/anthropic)"))
	fmt.Fprintf(&b, "deepseek-base-url: %s\n", orDefault(cfg.DeepSeekBaseURL, "(default https://api.deepseek.com/anthropic)"))
	fmt.Fprintf(&b, "anthropic-api-key: %s\n", mask(SecretAnthropicAPIKey))
	fmt.Fprintf(&b, "openai-api-key: %s\n", mask(SecretOpenAIAPIKey))
	fmt.Fprintf(&b, "glm-api-key: %s\n", mask(SecretGLMAPIKey))
	fmt.Fprintf(&b, "deepseek-api-key: %s\n", mask(SecretDeepSeekAPIKey))
	fmt.Fprintf(&b, "claude-skills: %s\n", orDefault(FormatNameList(cfg.ClaudeSkills), "(none)"))
	fmt.Fprintf(&b, "claude-mcp: %s\n", orDefault(FormatNameList(cfg.ClaudeMCP), "(none)"))
	fmt.Fprintf(&b, "builtin-plugins: %s\n", orDefault(FormatNameList(cfg.BuiltinPlugins), "(none)"))
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
	fmt.Fprintf(&b, "msg-plane-mode: %s\n", orDefault(cfg.MsgPlaneMode, "pg"))
	fmt.Fprintf(&b, "msg-plane-nats-url: %s\n", orDefault(cfg.MsgPlaneNATSURL, "(unset)"))
	fmt.Fprintf(&b, "msg-plane-subject: %s\n", orDefault(cfg.MsgPlaneSubject, "runs.dispatch"))
	fmt.Fprintf(&b, "checkpoint-pg-host: %s\n", orDefault(cfg.CheckpointPGHost, "(unset)"))
	fmt.Fprintf(&b, "checkpoint-pg-port: %s\n", orDefault(cfg.CheckpointPGPort, "5432"))
	fmt.Fprintf(&b, "checkpoint-pg-db: %s\n", orDefault(cfg.CheckpointPGDB, "woca"))
	fmt.Fprintf(&b, "checkpoint-pg-user: %s\n", orDefault(cfg.CheckpointPGUser, "(unset)"))
	fmt.Fprintf(&b, "memory-pg-host: %s\n", orDefault(cfg.MemoryPGHost, "(unset)"))
	fmt.Fprintf(&b, "memory-pg-port: %s\n", orDefault(cfg.MemoryPGPort, "5432"))
	fmt.Fprintf(&b, "memory-pg-db: %s\n", orDefault(cfg.MemoryPGDB, "woca"))
	fmt.Fprintf(&b, "memory-pg-user: %s\n", orDefault(cfg.MemoryPGUser, "(unset)"))
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
	case KeyThinkingBudget:
		return strconv.Itoa(cfg.ThinkingBudget)
	case KeyAnthropicBaseURL:
		return cfg.AnthropicBaseURL
	case KeyOpenAIBaseURL:
		return cfg.OpenAIBaseURL
	case KeyGLMBaseURL:
		return cfg.GLMBaseURL
	case KeyDeepSeekBaseURL:
		return cfg.DeepSeekBaseURL
	case KeyClaudeSkills:
		return FormatNameList(cfg.ClaudeSkills)
	case KeyClaudeMCP:
		return FormatNameList(cfg.ClaudeMCP)
	case KeyBuiltinPlugins:
		return FormatNameList(cfg.BuiltinPlugins)
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
	case KeyLadyMEndpoint:
		return cfg.LadyMEndpoint
	case KeyLadyMAPIUsername:
		return cfg.LadyMAPIUsername
	case KeyDebugLog:
		return strconv.FormatBool(cfg.DebugLog)
	case KeyMsgPlaneMode:
		return cfg.MsgPlaneMode
	case KeyMsgPlaneNATSURL:
		return cfg.MsgPlaneNATSURL
	case KeyMsgPlaneSubject:
		return cfg.MsgPlaneSubject
	case KeyCheckpointPGHost:
		return cfg.CheckpointPGHost
	case KeyCheckpointPGPort:
		return cfg.CheckpointPGPort
	case KeyCheckpointPGDB:
		return cfg.CheckpointPGDB
	case KeyCheckpointPGUser:
		return cfg.CheckpointPGUser
	case KeyMemoryPGHost:
		return cfg.MemoryPGHost
	case KeyMemoryPGPort:
		return cfg.MemoryPGPort
	case KeyMemoryPGDB:
		return cfg.MemoryPGDB
	case KeyMemoryPGUser:
		return cfg.MemoryPGUser
	}
	return ""
}

// ParseNameList decodes a JSON string-array setting value (used by
// claude-skills / claude-mcp).
func ParseNameList(v string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("expected a JSON array of names, got %q", v)
	}
	return out, nil
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
