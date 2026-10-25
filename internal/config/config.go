// Package config holds the user-facing configuration for the woca harness:
// provider/model selection, credentials, permission policy, and working
// directory.
package config

import (
	"fmt"
	"strings"

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
	// ProviderGLM rides Zhipu's Anthropic-compatible endpoint
	// (open.bigmodel.cn/api/anthropic), which accepts the Messages schema
	// including extended thinking.
	ProviderGLM Provider = "glm"
	// ProviderDeepSeek rides DeepSeek's Anthropic-compatible endpoint
	// (api.deepseek.com/anthropic); thinking is supported but the budget is
	// managed server-side.
	ProviderDeepSeek Provider = "deepseek"
)

// Providers lists the settable provider names.
func Providers() []Provider {
	return []Provider{ProviderAnthropic, ProviderOpenAI, ProviderGLM, ProviderDeepSeek}
}

// ProviderDefaultBaseURL is the API base a provider uses when the user has
// not configured one ("" = the adapter's own default).
func ProviderDefaultBaseURL(p Provider) string {
	switch p {
	case ProviderGLM:
		return "https://open.bigmodel.cn/api/anthropic"
	case ProviderDeepSeek:
		return "https://api.deepseek.com/anthropic"
	default:
		return ""
	}
}

// ProviderDefaultModel is the model a provider falls back to when none is
// configured.
func ProviderDefaultModel(p Provider) string {
	switch p {
	case ProviderOpenAI:
		return "gpt-4o"
	case ProviderGLM:
		return "glm-4.6"
	case ProviderDeepSeek:
		return "deepseek-chat"
	default:
		return "claude-sonnet-4-5"
	}
}

// ContextWindowFor returns the model's context-window limit in input tokens
// (0 = unknown; the UI then hides the percentage bar). It matches on the
// model name rather than the provider field — gateways commonly run one
// vendor's models behind another's API (deepseek-* on an OpenAI-compatible
// base URL), so the name is the reliable signal. Public vendor limits with
// conservative prefix matching; an unknown model reports 0 rather than a
// guess, since a wrong limit is worse than none for the usage display.
func ContextWindowFor(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}
	switch {
	case strings.HasPrefix(m, "claude"):
		return 200_000
	case strings.HasPrefix(m, "glm-4.5"):
		return 128_000
	case strings.HasPrefix(m, "glm-4.6"), strings.HasPrefix(m, "glm-4.7"), strings.HasPrefix(m, "glm-5"):
		return 200_000
	case strings.HasPrefix(m, "deepseek"):
		return 128_000
	case strings.HasPrefix(m, "gpt-5"):
		return 400_000
	case strings.HasPrefix(m, "gpt-4.1"):
		return 1_000_000
	case strings.HasPrefix(m, "gpt-4o"), strings.HasPrefix(m, "gpt-4-turbo"):
		return 128_000
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return 200_000
	}
	return 0
}

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
	// ThinkingBudget enables provider extended thinking when > 0. The
	// ladder is provider-mapped at model build time: Anthropic/GLM take it
	// as budget_tokens, DeepSeek as a plain thinking switch, and OpenAI as
	// reasoning_effort. ThinkingLevelsFor reports whether the current
	// model accepts it at all.
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
	case ProviderAnthropic, ProviderOpenAI, ProviderGLM, ProviderDeepSeek:
	default:
		return out, fmt.Errorf("config: unknown provider %q", out.Provider)
	}
	if out.Model == "" {
		out.Model = ProviderDefaultModel(out.Provider)
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
		// when the budget would swallow it (the two caps are linked).
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
// clamped to [1024, 31999] (MAX_THINKING_TOKENS semantics), which keeps the
// 64k escalation cap comfortably above any budget.
const (
	MinThinkingBudget      = 1024
	MaxThinkingBudget      = 31999
	ThinkingOutputHeadroom = 8192
)

// Thinking levels are the user-facing names for extended-thinking budgets
// (a think / think hard / ultrathink ladder, condensed to four).
const (
	ThinkingLevelOff    = "off"
	ThinkingLevelLow    = "low"
	ThinkingLevelMedium = "medium"
	ThinkingLevelHigh   = "high"
)

// Budgets behind the named thinking levels.
const (
	ThinkingBudgetLow    = 4096
	ThinkingBudgetMedium = 10240
	ThinkingBudgetHigh   = MaxThinkingBudget
)

// ThinkingBudgetFor maps a level name (off | low | medium | high) to its
// token budget. ok is false for unknown names.
func ThinkingBudgetFor(level string) (int, bool) {
	switch level {
	case ThinkingLevelOff:
		return 0, true
	case ThinkingLevelLow:
		return ThinkingBudgetLow, true
	case ThinkingLevelMedium:
		return ThinkingBudgetMedium, true
	case ThinkingLevelHigh:
		return ThinkingBudgetHigh, true
	}
	return 0, false
}

// ThinkingLevel maps a token budget back to its level name. Budgets between
// levels round down to the level they satisfy.
func ThinkingLevel(budget int) string {
	switch {
	case budget <= 0:
		return ThinkingLevelOff
	case budget >= ThinkingBudgetHigh:
		return ThinkingLevelHigh
	case budget >= ThinkingBudgetMedium:
		return ThinkingLevelMedium
	default:
		return ThinkingLevelLow
	}
}

// thinkingLevels is the full ladder; providers return a subset (or none when
// the model cannot think).
var thinkingLevels = []string{ThinkingLevelLow, ThinkingLevelMedium, ThinkingLevelHigh}

// ThinkingLevelsFor reports which thinking levels the provider's model
// accepts (the provider template's capability check). An empty result means
// the model has no thinking mode — callers should disable the thinking
// selector. Detection is by model-name pattern: the anthropic API gates
// thinking to claude 3.7+/4.x, OpenAI to the o-series / gpt-5 family
// (reasoning_effort), GLM to glm-4.5+, and DeepSeek accepts the thinking
// switch on every model (budget ignored, effort managed server-side).
func ThinkingLevelsFor(p Provider, model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return nil
	}
	switch p {
	case ProviderAnthropic:
		switch {
		case !strings.Contains(m, "claude"):
			return nil
		// 3.0 and 3.5 generations have no extended thinking.
		case strings.Contains(m, "claude-3-5"), isClaude3(m):
			return nil
		default:
			return thinkingLevels
		}
	case ProviderOpenAI:
		for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
			if strings.HasPrefix(m, prefix) {
				return thinkingLevels
			}
		}
		return nil
	case ProviderGLM:
		for _, prefix := range []string{"glm-4.5", "glm-4.6", "glm-4.7", "glm-5"} {
			if strings.HasPrefix(m, prefix) {
				return thinkingLevels
			}
		}
		return nil
	case ProviderDeepSeek:
		if strings.HasPrefix(m, "deepseek") {
			return thinkingLevels
		}
		return nil
	default:
		return nil
	}
}

// isClaude3 matches the claude-3.x generation without a minor version
// (claude-3-opus, claude-3-haiku-…, claude-sonnet-3-…); 3.7 spells itself
// claude-3-7-… and is caught by the prefix before this runs.
func isClaude3(model string) bool {
	for _, prefix := range []string{"claude-3-", "claude-opus-3", "claude-sonnet-3", "claude-haiku-3"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// ThinkingSupportNote explains, per provider, how levels map onto the
// provider's knobs (shown as the selector's hint).
func ThinkingSupportNote(p Provider) string {
	switch p {
	case ProviderOpenAI:
		return "OpenAI reasoning_effort"
	case ProviderGLM:
		return "GLM thinking (budget trimmed server-side)"
	case ProviderDeepSeek:
		return "DeepSeek thinking (no budget breakdown)"
	default:
		return ""
	}
}

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
	case ProviderAnthropic, ProviderGLM:
		// GLM rides the Anthropic-compatible endpoint, so its thinking rides
		// the same budget_tokens schema.
		am := anthropic.NewChatModel(opts...)
		if cfg.ThinkingBudget > 0 {
			am = am.WithThinking(map[string]any{
				"type":          "enabled",
				"budget_tokens": cfg.ThinkingBudget,
			})
		}
		return withName(am, cfg.Model), nil
	case ProviderDeepSeek:
		// DeepSeek's Anthropic-compatible endpoint accepts the thinking
		// switch but ignores budget_tokens, so only the type is sent.
		am := anthropic.NewChatModel(opts...)
		if cfg.ThinkingBudget > 0 {
			am = am.WithThinking(map[string]any{"type": "enabled"})
		}
		return withName(am, cfg.Model), nil
	case ProviderOpenAI:
		// The OpenAI partner defaults to the Responses API, whose input
		// schema (function_call_output items) is not understood by the
		// OpenAI-COMPATIBLE gateways users point base-url at (DeepSeek and
		// friends reject the tool-result shape with a 400). The classic
		// Chat Completions API is the lowest common denominator for every
		// compatible endpoint, including OpenAI itself, so it is the
		// default here.
		om := openai.NewChatModel(opts...).WithChatCompletions()
		if cfg.ThinkingBudget > 0 {
			// The budget ladder is translated to reasoning_effort; the
			// effort knob is what the Chat Completions API accepts.
			om = om.WithReasoningEffort(ThinkingLevel(cfg.ThinkingBudget))
		}
		return withName(om, cfg.Model), nil
	default:
		return nil, fmt.Errorf("config: unknown provider %q", cfg.Provider)
	}
}
