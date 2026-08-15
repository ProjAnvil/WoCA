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
	// ThinkingBudget enables provider extended thinking (request-side
	// budget, Claude Code's thinkingConfig) when > 0. The Anthropic API
	// requires the budget to be ≥1024 tokens and max_tokens to exceed it;
	// Normalize enforces both.
	ThinkingBudget int
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
	if out.ThinkingBudget > 0 {
		if out.ThinkingBudget < MinThinkingBudget {
			out.ThinkingBudget = MinThinkingBudget
		}
		if out.ThinkingBudget > MaxThinkingBudget {
			out.ThinkingBudget = MaxThinkingBudget
		}
		// The API requires max_tokens > budget_tokens; raise the output cap
		// when the budget would swallow it (Claude Code's linked caps).
		if out.MaxTokens <= out.ThinkingBudget {
			out.MaxTokens = out.ThinkingBudget + ThinkingOutputHeadroom
		}
	}
	if out.WorkDir == "" {
		out.WorkDir = "."
	}
	return out, nil
}

// Thinking-budget constraints (Anthropic extended thinking). The budget is
// clamped to [1024, 31999] — Claude Code's MAX_THINKING_TOKENS semantics —
// which keeps the 64k escalation cap comfortably above any budget.
const (
	MinThinkingBudget      = 1024
	MaxThinkingBudget      = 31999
	ThinkingOutputHeadroom = 8192
)

// BuildModel constructs a language.ChatModel from the configuration using the
// partner adapters in langchain-golang. When a thinking budget is set it also
// enforces the provider invariants (budget within [1024, 31999] and a
// max_tokens cap above the budget) even on un-normalized input, so every
// construction site stays valid.
func BuildModel(cfg Config) (language.ChatModel, error) {
	if cfg.ThinkingBudget > 0 {
		if cfg.ThinkingBudget < MinThinkingBudget {
			cfg.ThinkingBudget = MinThinkingBudget
		}
		if cfg.ThinkingBudget > MaxThinkingBudget {
			cfg.ThinkingBudget = MaxThinkingBudget
		}
		if cfg.MaxTokens <= cfg.ThinkingBudget {
			cfg.MaxTokens = cfg.ThinkingBudget + ThinkingOutputHeadroom
		}
	}
	opts := []modelconfig.Option{modelconfig.WithModel(cfg.Model)}
	if hc := modelHTTPClient(); hc != nil {
		// One connection per provider request: gateways that close keep-alive
		// connections after a stream would otherwise reset the next reused
		// request ("context canceled" mid-turn). See modelhttp.go.
		opts = append(opts, modelconfig.WithHTTPClient(hc))
	}
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
		am := anthropic.NewChatModel(opts...)
		if cfg.ThinkingBudget > 0 {
			am = am.WithThinking(map[string]any{
				"type":          "enabled",
				"budget_tokens": cfg.ThinkingBudget,
			})
		}
		return am, nil
	case ProviderOpenAI:
		// The OpenAI partner defaults to the Responses API, whose input
		// schema (function_call_output items) is not understood by the
		// OpenAI-COMPATIBLE gateways users point base-url at (DeepSeek and
		// friends reject the tool-result shape with a 400). The classic
		// Chat Completions API is the lowest common denominator for every
		// compatible endpoint, including OpenAI itself, so it is the
		// default here.
		return openai.NewChatModel(opts...).WithChatCompletions(), nil
	default:
		return nil, fmt.Errorf("config: unknown provider %q", cfg.Provider)
	}
}
