package config

import (
	"testing"
)

func TestProvidersAndDefaults(t *testing.T) {
	if got := Providers(); len(got) != 4 {
		t.Fatalf("Providers = %v", got)
	}
	for _, c := range []struct {
		p     Provider
		url   string
		model string
	}{
		{ProviderGLM, "https://open.bigmodel.cn/api/anthropic", "glm-4.6"},
		{ProviderDeepSeek, "https://api.deepseek.com/anthropic", "deepseek-chat"},
		{ProviderOpenAI, "", "gpt-4o"},
		{ProviderAnthropic, "", "claude-sonnet-4-5"},
	} {
		if got := ProviderDefaultBaseURL(c.p); got != c.url {
			t.Errorf("base URL %v = %q", c.p, got)
		}
		if got := ProviderDefaultModel(c.p); got != c.model {
			t.Errorf("model %v = %q", c.p, got)
		}
	}
	def := Default()
	if def.Provider != ProviderAnthropic || def.Model != "claude-sonnet-4-5" || def.MaxTokens != 8192 || def.RecursionLimit != 50 {
		t.Fatalf("Default = %+v", def)
	}
}

func TestThinkingLadder(t *testing.T) {
	for _, c := range []struct {
		level  string
		budget int
		ok     bool
	}{
		{ThinkingLevelOff, 0, true},
		{ThinkingLevelLow, ThinkingBudgetLow, true},
		{ThinkingLevelMedium, ThinkingBudgetMedium, true},
		{ThinkingLevelHigh, ThinkingBudgetHigh, true},
		{"mega", 0, false},
	} {
		b, ok := ThinkingBudgetFor(c.level)
		if ok != c.ok || (c.ok && b != c.budget) {
			t.Errorf("ThinkingBudgetFor(%q) = %d %v", c.level, b, ok)
		}
	}
	for _, c := range []struct {
		budget int
		want   string
	}{
		{0, ThinkingLevelOff},
		{-5, ThinkingLevelOff},
		{1024, ThinkingLevelLow},
		{ThinkingBudgetMedium, ThinkingLevelMedium},
		{ThinkingBudgetHigh, ThinkingLevelHigh},
		{99999, ThinkingLevelHigh},
	} {
		if got := ThinkingLevel(c.budget); got != c.want {
			t.Errorf("ThinkingLevel(%d) = %q", c.budget, got)
		}
	}
	// Levels per provider/model pattern.
	if got := ThinkingLevelsFor(ProviderAnthropic, ""); got != nil {
		t.Errorf("empty model = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderAnthropic, "claude-sonnet-4-5"); len(got) != 3 {
		t.Errorf("sonnet levels = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderAnthropic, "claude-3-5-sonnet"); got != nil {
		t.Errorf("claude3 levels = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderOpenAI, "gpt-4o"); got != nil {
		t.Errorf("gpt-4o levels = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderOpenAI, "o3"); len(got) == 0 {
		t.Errorf("o3 levels = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderGLM, "glm-4.6"); len(got) == 0 {
		t.Errorf("glm levels = %v", got)
	}
	if got := ThinkingLevelsFor(ProviderDeepSeek, "deepseek-chat"); len(got) == 0 {
		t.Errorf("deepseek levels = %v", got)
	}
	for _, m := range []string{"claude-3-opus", "claude-opus-3", "claude-sonnet-3", "claude-haiku-3"} {
		if !isClaude3(m) {
			t.Errorf("isClaude3(%q) = false", m)
		}
	}
	if isClaude3("claude-sonnet-4-5") {
		t.Error("4.5 is not claude3")
	}
	for _, c := range []struct {
		p    Provider
		want string
	}{
		{ProviderOpenAI, "OpenAI reasoning_effort"},
		{ProviderGLM, "GLM"},
		{ProviderDeepSeek, "DeepSeek"},
		{ProviderAnthropic, ""},
	} {
		note := ThinkingSupportNote(c.p)
		if (c.want == "" && note != "") || (c.want != "" && len(note) < len(c.want)) {
			t.Errorf("note %v = %q", c.p, note)
		}
	}
}
