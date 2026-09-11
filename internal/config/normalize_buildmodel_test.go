package config

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
)

func TestNormalizeUnknownProvider(t *testing.T) {
	if _, err := (Config{Provider: Provider("weird")}).Normalize(); err == nil {
		t.Fatal("Normalize accepted an unknown provider")
	}
	if _, err := BuildModel(Config{Provider: Provider("weird")}); err == nil {
		t.Fatal("BuildModel accepted an unknown provider")
	}
}

func TestNormalizeDefaultMaxTokens(t *testing.T) {
	n, err := Config{Provider: ProviderOpenAI}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if n.MaxTokens != 8192 {
		t.Fatalf("MaxTokens = %d, want default 8192", n.MaxTokens)
	}
	if n.Provider != ProviderOpenAI || n.Model != "gpt-4o" || n.PermissionMode != ModeDefault ||
		n.RecursionLimit != 50 || n.WorkDir != "." {
		t.Fatalf("defaults not filled: %+v", n)
	}
}

func TestNormalizeClampsHighThinkingBudget(t *testing.T) {
	n, err := Config{Provider: ProviderAnthropic, ThinkingBudget: 500000, MaxTokens: 8192}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if n.ThinkingBudget != MaxThinkingBudget {
		t.Fatalf("budget = %d, want clamp to %d", n.ThinkingBudget, MaxThinkingBudget)
	}
	if n.MaxTokens != MaxThinkingBudget+ThinkingOutputHeadroom {
		t.Fatalf("MaxTokens = %d, want budget+headroom", n.MaxTokens)
	}
}

func TestThinkingLevelsForNegativePatterns(t *testing.T) {
	if got := ThinkingLevelsFor(ProviderAnthropic, "gpt-4o"); got != nil {
		t.Errorf("anthropic non-claude = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderGLM, "glm-4"); got != nil {
		t.Errorf("glm-4 = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderDeepSeek, "llama-3"); got != nil {
		t.Errorf("deepseek non-deepseek = %v", got)
	}
	if got := ThinkingLevelsFor(Provider("nope"), "claude-sonnet-4-5"); got != nil {
		t.Errorf("unknown provider = %v", got)
	}
}

func TestBuildModelTemperatureOption(t *testing.T) {
	m, err := BuildModel(Config{Provider: ProviderOpenAI, Model: "gpt-4o", Temperature: 0.7})
	if err != nil || m == nil {
		t.Fatalf("build with temperature: %v %v", m, err)
	}
}

func TestBuildModelDeepSeekThinkingSwitchAndClamp(t *testing.T) {
	// DeepSeek: the budget is not sent, only the enabled switch.
	bodies := invokeOnce(t, Config{Provider: ProviderDeepSeek, Model: "deepseek-chat", ThinkingBudget: 2048, MaxTokens: 8192})
	thinking, _ := (*bodies)[0]["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" || len(thinking) != 1 {
		t.Fatalf("deepseek thinking = %v, want {type: enabled} only", thinking)
	}

	// A sub-minimum budget is clamped before it reaches the wire.
	bodies = invokeOnce(t, Config{Provider: ProviderAnthropic, Model: "claude-test", ThinkingBudget: 10, MaxTokens: 9000})
	thinking, _ = (*bodies)[0]["thinking"].(map[string]any)
	if thinking == nil || int(thinking["budget_tokens"].(float64)) != MinThinkingBudget {
		t.Fatalf("clamped thinking = %v, want budget %d", thinking, MinThinkingBudget)
	}
}

func TestModelHTTPClientKeepaliveEnv(t *testing.T) {
	if c := modelHTTPClient(); c == nil {
		t.Fatal("default client must disable keep-alives, not be nil")
	}
	t.Setenv("WOCA_KEEPALIVE_MODELS", "1")
	if c := modelHTTPClient(); c != nil {
		t.Fatalf("keepalive env override = %+v, want nil", c)
	}
}

func TestWithNamePassthrough(t *testing.T) {
	inner := namedModel{ChatModel: language.NewFakeChatModel(), name: "original"}
	got := withName(inner, "replacement")
	nm, ok := got.(namedModel)
	if !ok {
		t.Fatalf("withName = %T, want namedModel", got)
	}
	if nm.name != "original" {
		t.Fatalf("withName rewrapped a model that already has a name: %q", nm.name)
	}
	if nm.Name() != "original" {
		t.Fatalf("Name() = %q, want original", nm.Name())
	}
}
