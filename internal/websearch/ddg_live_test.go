package websearch

import (
	"os"
	"testing"
)

// TestDDGLive exercises the real DuckDuckGo endpoint and is skipped by default.
//
// It exists because the fixture-based tests cannot answer the only question
// that matters for the default backend: does it work from THIS network?
// DuckDuckGo blocks by IP reputation, and datacenter ranges are refused
// deterministically, so a residential laptop passing proves nothing about a CI
// runner or an enterprise worker. Run it in the target environment before
// relying on the default chain:
//
//	WOCA_SEARCH_LIVE=1 go test ./internal/websearch/ -run TestDDGLive -v
func TestDDGLive(t *testing.T) {
	if os.Getenv("WOCA_SEARCH_LIVE") != "1" {
		t.Skip("set WOCA_SEARCH_LIVE=1 to query the real DuckDuckGo endpoint")
	}

	for _, p := range []Provider{NewDDGHTML(DDGOptions{}), NewDDGLite(DDGOptions{})} {
		t.Run(p.ID(), func(t *testing.T) {
			results, err := p.Search(t.Context(), "golang mcp server", 5)
			if err != nil {
				// A rate-limit error here is a real finding, not a flake:
				// it means this network cannot use this backend.
				t.Fatalf("%s: %v", p.ID(), err)
			}
			if len(results) == 0 {
				t.Fatalf("%s: 0 results; selectors may have drifted", p.ID())
			}
			for _, r := range results {
				t.Logf("%s | %s", r.Title, r.URL)
			}
		})
	}
}
