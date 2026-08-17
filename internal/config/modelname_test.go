package config

import (
	"testing"
)

// The agent keys per-workspace prompt caches by the active model's name and
// the system prompt names the model, so BuildModel must return a model that
// reports its configured id via Name().
func TestBuildModelNamesModel(t *testing.T) {
	for _, provider := range []Provider{ProviderAnthropic, ProviderGLM, ProviderDeepSeek, ProviderOpenAI} {
		cfg := Config{Provider: provider, Model: "test-model-id", APIKey: "k"}
		m, err := BuildModel(cfg)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		named, ok := m.(interface{ Name() string })
		if !ok {
			t.Fatalf("%s: model %T does not implement Name()", provider, m)
		}
		if got := named.Name(); got != "test-model-id" {
			t.Fatalf("%s: Name() = %q, want test-model-id", provider, got)
		}
	}
}
