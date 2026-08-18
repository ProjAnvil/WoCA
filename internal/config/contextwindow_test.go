package config

import "testing"

// TestContextWindowFor verifies the context-window lookup: known models map
// to their public limits regardless of the provider they run behind (gateways
// commonly host one vendor's models on another's API), unknown models report
// 0 (the UI hides the bar).
func TestContextWindowFor(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-sonnet-4-5", 200_000},
		{"claude-opus-4-1", 200_000},
		{"glm-4.5", 128_000},
		{"glm-4.5-air", 128_000},
		{"glm-4.6", 200_000},
		{"deepseek-chat", 128_000},
		// DeepSeek behind an OpenAI-compatible gateway still resolves.
		{"deepseek-v4-pro", 128_000},
		{"gpt-5", 400_000},
		{"gpt-5-codex", 400_000},
		{"gpt-4.1", 1_000_000},
		{"gpt-4o-mini", 128_000},
		{"o3-mini", 200_000},
		// Unknowns report 0 rather than a guess.
		{"some-future-model", 0},
		{"", 0},
		{"  ", 0},
	}
	for _, c := range cases {
		if got := ContextWindowFor(c.model); got != c.want {
			t.Errorf("ContextWindowFor(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
