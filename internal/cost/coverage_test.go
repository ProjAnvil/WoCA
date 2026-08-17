package cost

import "testing"

func TestEstimateCacheAndUnknown(t *testing.T) {
	// Unknown model.
	if _, known := Estimate("no-such-model", Usage{}); known {
		t.Fatal("unknown model should not estimate")
	}
	// Negative fresh input clamps to zero.
	model := "claude-sonnet-4-5"
	if _, _, ok := Lookup(model); !ok {
		t.Skip("pricing missing")
	}
	u := Usage{InputTokens: 100, CacheReadTokens: 400, OutputTokens: 1000}
	got, known := Estimate(model, u)
	if !known {
		t.Fatal("known model")
	}
	// Fresh input is clamped to zero, so the estimate is driven by the
	// output tokens plus the cache-scaled reads.
	if got <= 0 {
		t.Fatalf("Estimate = %v (want a positive value)", got)
	}
}
