package sdk

import (
	"io"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/platform/executor"
)

// Options configures one headless run. The zero value is the CLI's default
// posture with headless-safe interrupt handling (everything denied).
type Options struct {
	Model          string
	Provider       string // anthropic | openai | glm | deepseek
	APIKey         string
	BaseURL        string
	Workdir        string
	ExtraPrompt    string // appended to the system prompt
	PermissionMode string // default | acceptEdits | bypassPermissions | readOnly | plan
	MaxTurns       int
	MaxTokens      int
	ThinkingBudget int
	Temperature    float64
	MCPServers     []string // name=command or name=http(s)://endpoint
	AllowCommands  []string
	DenyCommands   []string
	DB             string // sqlite path, postgres:// DSN, or "none"
	FallbackModel  string
	CheckpointDir  string
	// OnInterrupt overrides the headless default (deny everything). The
	// returned value is the graph resume value — see package docs.
	OnInterrupt func(Interrupt) (any, error)
	// Plugins are processing plugins (see Plugin), injected at
	// construction and observe-only today: EventPlugins receive the run
	// lifecycle in stream order.
	Plugins []Plugin
	// Telemetry sinks receive harness spans (LLM calls, tool invocations,
	// skill loads) — see TelemetrySink. Installed process-wide, deduped by
	// identity; a host typically installs once and reuses across queries.
	Telemetry []TelemetrySink
	// LogOutput redirects the embedded agent's logs (silent by default).
	LogOutput io.Writer
	// LogLevel names a zerolog level ("debug", "info", "warn", "error");
	// empty is "error".
	LogLevel string
	// Sandbox, when set, routes every process side effect (shell and file
	// tools, hooks, MCP stdio servers, plugin runtimes) through the remote
	// sandbox executor at endpoint instead of the host.
	Sandbox string
	// SandboxAPIKey authenticates the sandbox executor (optional).
	SandboxAPIKey string
}

// Option mutates Options (functional-option style).
type Option func(*Options)

func WithModel(v string) Option            { return func(o *Options) { o.Model = v } }
func WithProvider(v string) Option         { return func(o *Options) { o.Provider = v } }
func WithAPIKey(v string) Option           { return func(o *Options) { o.APIKey = v } }
func WithBaseURL(v string) Option          { return func(o *Options) { o.BaseURL = v } }
func WithWorkdir(v string) Option          { return func(o *Options) { o.Workdir = v } }
func WithSystemPrompt(v string) Option     { return func(o *Options) { o.ExtraPrompt = v } }
func WithPermissionMode(v string) Option   { return func(o *Options) { o.PermissionMode = v } }
func WithMaxTurns(v int) Option            { return func(o *Options) { o.MaxTurns = v } }
func WithMaxTokens(v int) Option           { return func(o *Options) { o.MaxTokens = v } }
func WithThinkingBudget(v int) Option      { return func(o *Options) { o.ThinkingBudget = v } }
func WithTemperature(v float64) Option     { return func(o *Options) { o.Temperature = v } }
func WithMCPServers(v ...string) Option    { return func(o *Options) { o.MCPServers = v } }
func WithAllowCommands(v ...string) Option { return func(o *Options) { o.AllowCommands = v } }
func WithDenyCommands(v ...string) Option  { return func(o *Options) { o.DenyCommands = v } }
func WithDB(v string) Option               { return func(o *Options) { o.DB = v } }
func WithFallbackModel(v string) Option    { return func(o *Options) { o.FallbackModel = v } }
func WithCheckpointDir(v string) Option    { return func(o *Options) { o.CheckpointDir = v } }
func WithInterruptHandler(f func(Interrupt) (any, error)) Option {
	return func(o *Options) { o.OnInterrupt = f }
}
func WithPlugins(ps ...Plugin) Option {
	return func(o *Options) { o.Plugins = append(o.Plugins, ps...) }
}
func WithTelemetry(s ...TelemetrySink) Option {
	return func(o *Options) { o.Telemetry = append(o.Telemetry, s...) }
}
func WithLogOutput(w io.Writer) Option { return func(o *Options) { o.LogOutput = w } }
func WithLogLevel(l string) Option     { return func(o *Options) { o.LogLevel = l } }
func WithSandbox(endpoint, apiKey string) Option {
	return func(o *Options) { o.Sandbox, o.SandboxAPIKey = endpoint, apiKey }
}

// buildOptions translates into the composition-root options. LogLevel is
// pinned to error: an embedded agent must not spam the host process (the
// logger itself is redirected by logFor, not by this field).
func (o Options) buildOptions(prompt string) agentbuild.Options {
	var sandbox *executor.SandboxConfig
	if o.Sandbox != "" {
		sandbox = &executor.SandboxConfig{Endpoint: o.Sandbox, APIKey: o.SandboxAPIKey}
	}
	return agentbuild.Options{
		Prompt:         prompt,
		Model:          o.Model,
		Provider:       o.Provider,
		APIKey:         o.APIKey,
		BaseURL:        o.BaseURL,
		MaxTokens:      o.MaxTokens,
		ThinkingBudget: o.ThinkingBudget,
		Temperature:    o.Temperature,
		Mode:           o.PermissionMode,
		Workdir:        o.Workdir,
		ExtraPrompt:    o.ExtraPrompt,
		LogLevel:       "error",
		MCPServers:     o.MCPServers,
		FallbackModel:  o.FallbackModel,
		CheckpointDir:  o.CheckpointDir,
		AllowCommands:  o.AllowCommands,
		DenyCommands:   o.DenyCommands,
		DB:             o.DB,
		MaxTurns:       o.MaxTurns,
		ToolGuards:     toolGuardsOf(o.Plugins),
		ModelHooks:     modelHooksOf(o.Plugins),
		Sandbox:        sandbox,
	}
}
