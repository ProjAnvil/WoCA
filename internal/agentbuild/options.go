// Package agentbuild is the composition root shared by every entry point
// that runs the agent (the personal terminal frontends, serve, and the
// future server/worker roles): it turns command-line options into a fully
// assembled agent application — model, prompt, hooks, skills, MCP, plugins,
// storage backends — with all process side effects routed through one
// executor.Executor.
//
// Split rationale (microservices design doc §5.1/§13 Phase 0): subcommand
// groups import only what they need, so future cmd/ entries can assemble
// smaller binaries (the server roles never link the terminal frontends, and
// only UI-serving entries link the embedded SPA).
package agentbuild

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/spf13/pflag"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/settings"
)

// Options carries the agent configuration shared by every frontend. The
// fields are written by RegisterFlags from the root command's persistent
// flags.
type Options struct {
	Prompt           string
	Model            string
	Provider         string
	APIKey           string
	BaseURL          string
	MaxTokens        int
	ThinkingBudget   int
	OutputFormat     string
	ManagedSettings  string
	Temperature      float64
	Mode             string
	SkipPerms        bool
	AcceptEdits      bool
	ReadOnly         bool
	Plan             bool
	Workdir          string
	ExtraPrompt      string
	Resume           string
	LogLevel         string
	CompactLimit     int
	MCPServers       []string
	FallbackModel    string
	SearchAPI        string
	SearchAPIKey     string
	CheckpointDir    string
	ResumeCheckpoint string
	ResumeValue      string
	AllowCommands    []string
	DenyCommands     []string
	DB               string
	MaxTurns         int
	DebugLog         bool
	// EnterpriseMemory marks a multi-tenant deployment (serve --auth
	// apikey). The embedded ladyM engine is then forbidden: memory is the
	// remote ladyM enterprise client or none (design §6.3.2/§6.3.8).
	EnterpriseMemory bool
	// OnSettingsReady runs once the settings store exists but before any
	// setting is read or any build-time connection opens — the bridge form
	// installs its managed overlay here with a blocking initial config
	// pull, so the postgres/remote-memory coordinates the config plane
	// delivers are already effective when the storage backends build. An
	// error is logged and never fatal: standalone operation must survive a
	// dead channel.
	OnSettingsReady func(ctx context.Context, st *settings.Store) error
	// Sandbox, when set, routes every process side effect (shell and file
	// tools, hooks, MCP stdio servers, plugin runtimes) through the remote
	// sandbox executor instead of the host (design §8). Nil keeps the local
	// executor — the personal-edition posture. See SandboxFromEnv.
	Sandbox *executor.SandboxConfig
	// ToolGuards/ModelHooks are the in-process processing plugins passed
	// through to the agent (the sdk's plugin surface). Nil keeps the
	// un-instrumented paths byte-identical.
	ToolGuards []agent.ToolGuard
	ModelHooks []agent.ModelHook
}

// SandboxFromEnv resolves the sandbox executor configuration from the
// environment (WOCA_SANDBOX_ENDPOINT, WOCA_SANDBOX_API_KEY,
// WOCA_SANDBOX_TIMEOUT_MS). It returns nil — the local-executor posture —
// unless an endpoint is set, so every entry point can call it
// unconditionally.
func SandboxFromEnv() *executor.SandboxConfig {
	endpoint := os.Getenv("WOCA_SANDBOX_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	cfg := &executor.SandboxConfig{
		Endpoint: endpoint,
		APIKey:   os.Getenv("WOCA_SANDBOX_API_KEY"),
	}
	if ms, err := strconv.Atoi(os.Getenv("WOCA_SANDBOX_TIMEOUT_MS")); err == nil && ms > 0 {
		cfg.Timeout = time.Duration(ms) * time.Millisecond
	}
	return cfg
}

// RegisterFlags binds every option to fl. It is used for the root command's
// persistent flags so the agent configuration can be set after the
// subcommand name as well.
func RegisterFlags(fl *pflag.FlagSet, o *Options) {
	fl.StringVarP(&o.Prompt, "prompt", "p", "", "one-shot prompt; runs non-interactively and exits")
	fl.StringVar(&o.Model, "model", "", "model name (e.g. claude-sonnet-4-5, gpt-4o)")
	fl.StringVar(&o.Provider, "provider", "", "provider: anthropic or openai")
	fl.StringVar(&o.APIKey, "api-key", "", "API key (falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY)")
	fl.StringVar(&o.BaseURL, "base-url", "", "provider base URL")
	fl.IntVar(&o.MaxTokens, "max-tokens", 0, "maximum output tokens")
	fl.IntVar(&o.ThinkingBudget, "thinking-budget", 0, "extended-thinking budget in tokens (0 = off; anthropic provider only, minimum 1024)")
	fl.StringVar(&o.OutputFormat, "output-format", "", "headless output format for --prompt runs: json or stream-json (default plain text)")
	fl.StringVar(&o.ManagedSettings, "managed-settings", "", "enterprise managed settings file (overrides and locks settings; env WOCA_MANAGED_SETTINGS)")
	fl.Float64Var(&o.Temperature, "temperature", 0, "sampling temperature")
	fl.StringVar(&o.Mode, "permission-mode", "", "default | acceptEdits | bypassPermissions | readOnly | plan")
	fl.BoolVar(&o.SkipPerms, "dangerously-skip-permissions", false, "allow all tools without prompting")
	fl.BoolVar(&o.AcceptEdits, "accept-edits", false, "auto-accept file edits; still prompt before shell commands")
	fl.BoolVar(&o.ReadOnly, "read-only", false, "deny write/shell tools")
	fl.BoolVar(&o.Plan, "plan", false, "read-only planning mode")
	fl.StringVar(&o.Workdir, "workdir", "", "working directory (default: current directory)")
	fl.StringVar(&o.ExtraPrompt, "system-prompt", "", "extra instructions appended to the system prompt")
	fl.StringVar(&o.Resume, "resume", "", "resume from a session transcript file (or \"last\")")
	fl.StringVar(&o.LogLevel, "log-level", "warn", "log level: trace | debug | info | warn | error | disabled")
	fl.IntVar(&o.CompactLimit, "auto-compact", 0, "auto-compact the conversation above this token estimate (0 disables)")
	fl.StringArrayVar(&o.MCPServers, "mcp", nil, "MCP server as name=command or name=http(s)://endpoint (repeatable)")
	fl.StringVar(&o.FallbackModel, "fallback-model", "", "fallback model name used after transient retries fail")
	fl.StringVar(&o.SearchAPI, "search-api", "", "JSON web-search API endpoint for WebSearch (with {q} placeholder)")
	fl.StringVar(&o.SearchAPIKey, "search-api-key", "", "bearer key for --search-api")
	fl.StringVar(&o.CheckpointDir, "checkpoint-dir", "", "directory to persist graph checkpoints (enables cross-process interrupt/resume)")
	fl.StringVar(&o.ResumeCheckpoint, "resume-checkpoint", "", "resume a paused run by its checkpoint thread ID (durable saver required)")
	fl.StringVar(&o.ResumeValue, "resume-value", "", "resume value for --resume-checkpoint (e.g. y/n)")
	fl.StringArrayVar(&o.AllowCommands, "allow-command", nil, "Bash command prefix to allow (repeatable); when set, only these may run")
	fl.StringArrayVar(&o.DenyCommands, "deny-command", nil, "Bash command substring to block (repeatable)")
	fl.IntVar(&o.MaxTurns, "max-turns", 0, "cap the model rounds per turn (0 = unlimited)")
	fl.StringVar(&o.DB, "db", "", "settings database: sqlite path, postgres:// DSN, or \"none\" (default ~/.woca/woca.db; env WOCA_DB)")
	fl.BoolVar(&o.DebugLog, "debug-log", false, "enable the harness observability log (graph/state/session/message traces to ~/.woca/logs; also settable via settings debug-log)")
}
