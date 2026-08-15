// Package config holds the user-facing configuration for the woca harness:
// provider/model selection, credentials, permission policy, and working
// directory.
package config

import (
	"fmt"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/modelconfig"
	"github.com/projanvil/langchain-golang/partners/anthropic"
	"github.com/projanvil/langchain-golang/partners/openai"
)

// Provider identifies a model provider backend.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai"
)

// PermissionMode controls how tool calls are authorized.
type PermissionMode string

const (
	// ModeDefault asks for confirmation before write/shell tools.
	ModeDefault PermissionMode = "default"
	// ModeAcceptEdits auto-accepts file edits (Write/Edit) but still asks
	// before shell commands.
	ModeAcceptEdits PermissionMode = "acceptEdits"
	// ModeBypass allows every tool without prompting.
	ModeBypass PermissionMode = "bypassPermissions"
	// ModeReadOnly denies write/shell tools entirely.
	ModeReadOnly PermissionMode = "readOnly"
	// ModePlan is read-only plus a planning hint.
	ModePlan PermissionMode = "plan"
)

// Config is the complete harness configuration.
type Config struct {
	Provider       Provider
	Model          string
	APIKey         string
	BaseURL        string
	MaxTokens      int
	Temperature    float64
	PermissionMode PermissionMode
	WorkDir        string
	RecursionLimit int
	Debug          bool
	// ExtraSystemPrompt is appended to the base coding-agent system prompt.
	ExtraSystemPrompt string
}

// Default returns a sane default configuration. The model name is a
// placeholder; callers should override it (or it can be left for providers
// that have a default).
func Default() Config {
	return Config{
		Provider:       ProviderAnthropic,
		Model:          "claude-sonnet-4-5",
		MaxTokens:      8192,
		PermissionMode: ModeDefault,
		RecursionLimit: 50,
	}
}

// Normalize fills in zero-value fields with defaults and validates the
// configuration.
func (c Config) Normalize() (Config, error) {
	out := c
	if out.Provider == "" {
		out.Provider = ProviderAnthropic
	}
	switch out.Provider {
	case ProviderAnthropic, ProviderOpenAI:
	default:
		return out, fmt.Errorf("config: unknown provider %q", out.Provider)
	}
	if out.Model == "" {
		out.Model = "claude-sonnet-4-5"
	}
	if out.PermissionMode == "" {
		out.PermissionMode = ModeDefault
	}
	if out.RecursionLimit <= 0 {
		out.RecursionLimit = 50
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 8192
	}
	if out.WorkDir == "" {
		out.WorkDir = "."
	}
	return out, nil
}

// BuildModel constructs a language.ChatModel from the configuration using the
// partner adapters in langchain-golang.
func BuildModel(cfg Config) (language.ChatModel, error) {
	opts := []modelconfig.Option{modelconfig.WithModel(cfg.Model)}
	if cfg.APIKey != "" {
		opts = append(opts, modelconfig.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, modelconfig.WithBaseURL(cfg.BaseURL))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, modelconfig.WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Temperature > 0 {
		opts = append(opts, modelconfig.WithTemperature(cfg.Temperature))
	}
	switch cfg.Provider {
	case ProviderAnthropic:
		return anthropic.NewChatModel(opts...), nil
	case ProviderOpenAI:
		return openai.NewChatModel(opts...), nil
	default:
		return nil, fmt.Errorf("config: unknown provider %q", cfg.Provider)
	}
}
