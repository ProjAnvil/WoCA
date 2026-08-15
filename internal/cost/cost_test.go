package cost

import (
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-5-20250929", "Claude Sonnet"},
		{"anthropic/claude-opus-4-1", "Claude Opus"},
		{"claude-3-5-haiku-20241022", "Claude Haiku"},
		{"gpt-4o-2024-08-06", "GPT-4o"},
		{"gpt-4o-mini", "GPT-4o mini"},
		{"gpt-5.1", "GPT-5"},
		{"glm-4.6", "GLM-4"},
		{"totally-unknown", ""},
	}
	for _, tc := range cases {
		if _, got, ok := Lookup(tc.model); (got != tc.want) || (tc.want == "") != (!ok) {
			t.Errorf("Lookup(%q) = %q ok=%v, want %q ok=%v", tc.model, got, ok, tc.want, tc.want != "")
		}
	}
}

func TestEstimateSonnetWithCache(t *testing.T) {
	// 40k fresh + 60k cache read (×0.1) + 5k out = 0.12 + 0.018 + 0.075.
	c, known := Estimate("claude-sonnet-4-5", Usage{
		InputTokens:     100000,
		OutputTokens:    5000,
		CacheReadTokens: 60000,
	})
	if !known {
		t.Fatal("sonnet should be known")
	}
	if diff := c - 0.213; diff < -0.0005 || diff > 0.0005 {
		t.Fatalf("estimate = %.4f, want ≈0.213", c)
	}
}

func TestEstimateNoCacheFactors(t *testing.T) {
	// GPT pricing has no cache factors: cache reads price like fresh input.
	// 500k fresh in + 500k cache in @ $2.5/M + 1M out @ $10/M = $12.50.
	c, _ := Estimate("gpt-4o", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 500000})
	if diff := c - 12.5; diff < -0.0005 || diff > 0.0005 {
		t.Fatalf("estimate = %.4f, want 12.5", c)
	}
}

func TestFormatUSD(t *testing.T) {
	if got := FormatUSD(0); got != "$0.00" {
		t.Errorf("zero = %q", got)
	}
	if got := FormatUSD(0.0042); got != "$0.0042" {
		t.Errorf("tiny = %q", got)
	}
	if got := FormatUSD(1.5); got != "$1.50" {
		t.Errorf("normal = %q", got)
	}
	if !strings.HasPrefix(FormatUSD(-0.0), "$") {
		t.Error("format should always start with $")
	}
}
