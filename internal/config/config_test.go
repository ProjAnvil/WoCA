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
