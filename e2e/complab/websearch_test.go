//go:build complab

package complab

import (
	"context"
	"testing"

	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/websearch"
)

// searchQuery is common enough that an empty result set is unlikely.
//
// The result contents are deliberately NOT asserted: search results change, and
// asserting on them only raises a flaky test. What this proves is that the
// component returns results at all.
const searchQuery = "golang programming language"

// TestWebSearchReturnsResults is the headline component: call WebSearch the way
// the agent does and end up with results.
//
// It does not care which path served the query — free backends come and go with
// the network, so pinning a provider would make this test misfire on every
// backend switch.
func TestWebSearchReturnsResults(t *testing.T) {
	requireNetwork(t)
	log := installEventLog(t)

	tool := toolNamed(t, newRegistry(t), "WebSearch")
	content, err := invoke(t, tool, map[string]any{"query": searchQuery})
	if err != nil {
		explainSearchFailure(t, err, log)
	}

	if n := countResults(content); n == 0 {
		t.Fatalf("search for %q returned 0 results.\noutput: %q\nevent trail: %s",
			searchQuery, truncate(content, 400), log.summary())
	}
	urls := resultURLs(content)
	if len(urls) == 0 {
		t.Fatalf("no parseable URL in the results.\noutput: %q", truncate(content, 400))
	}
	t.Logf("search %q -> %d results, served by %q", searchQuery, countResults(content), log.servedBy())
	for _, u := range urls {
		t.Logf("  %s", u)
	}
}

// blockedProvider always reports "rate limited". It is used to prove the chain
// actually routes around a broken backend — which is what "don't care which
// path serves it" means in practice: the first path is down, the request still
// produces results.
type blockedProvider struct{ calls int }

func (p *blockedProvider) ID() string      { return "complab-blocked" }
func (p *blockedProvider) Name() string    { return "always blocked (complab probe)" }
func (p *blockedProvider) Available() bool { return true }

func (p *blockedProvider) Search(context.Context, string, int) ([]websearch.Result, error) {
	p.calls++
	return nil, &websearch.RateLimitError{Provider: p.ID(), Reason: "complab: deliberately blocked"}
}

func TestWebSearchSurvivesABlockedProvider(t *testing.T) {
	requireNetwork(t)
	log := installEventLog(t)

	blocked := &blockedProvider{}
	// Put the broken backend first: if failover does not work, this test gets 0
	// results.
	chain := append([]websearch.Provider{blocked}, websearch.LoadConfig().Build()...)
	if len(chain) < 2 {
		t.Fatal("default chain is empty, cannot exercise failover")
	}

	tool := tools.WebSearchToolWithProviders(chain...)
	content, err := invoke(t, tool, map[string]any{"query": searchQuery})
	if err != nil {
		explainSearchFailure(t, err, log)
	}
	if n := countResults(content); n == 0 {
		t.Fatalf("no failover after the first backend was blocked, got 0 results.\nevent trail: %s", log.summary())
	}
	if blocked.calls != 1 {
		t.Errorf("blocked backend was called %d times, want 1 (never retry a challenge page; just move on)", blocked.calls)
	}
	if served := log.servedBy(); served == blocked.ID() {
		t.Errorf("served by the blocked backend (%s), so failover did not really happen", served)
	}
	t.Logf("first backend blocked -> %q took over, %d results", log.servedBy(), countResults(content))
}

// TestWebFetchReturnsText verifies the fetch component really returns body text.
func TestWebFetchReturnsText(t *testing.T) {
	requireNetwork(t)

	tool := toolNamed(t, newRegistry(t), "WebFetch")
	content, err := invoke(t, tool, map[string]any{"url": "https://go.dev/doc/"})
	if err != nil {
		t.Fatalf("WebFetch failed: %v", err)
	}
	if content == "(empty page)" {
		t.Fatal("WebFetch returned the empty-page marker")
	}
	if len(content) < 200 {
		t.Fatalf("WebFetch returned only %d bytes, which does not look like a real page: %q",
			len(content), truncate(content, 200))
	}
	t.Logf("WebFetch returned %d bytes", len(content))
}

// TestWebSearchThenFetch is the combination check: the URLs a search returns
// must actually be fetchable.
//
// Green search on its own and green fetch on its own do not prove the searched
// links are usable — and searching then fetching is exactly how the agent uses
// them.
func TestWebSearchThenFetch(t *testing.T) {
	requireNetwork(t)
	log := installEventLog(t)

	reg := newRegistry(t)
	search := toolNamed(t, reg, "WebSearch")
	fetch := toolNamed(t, reg, "WebFetch")

	content, err := invoke(t, search, map[string]any{"query": searchQuery})
	if err != nil {
		explainSearchFailure(t, err, log)
	}
	urls := resultURLs(content)
	if len(urls) == 0 {
		t.Fatalf("search returned no URLs: %q", truncate(content, 400))
	}

	// Search results can contain PDFs or login walls, so give the first few
	// URLs a chance; one success is enough to show the combination works.
	var lastErr error
	for _, u := range urls[:min(3, len(urls))] {
		body, err := invoke(t, fetch, map[string]any{"url": u})
		if err != nil {
			lastErr = err
			t.Logf("skipped %s: %v", u, err)
			continue
		}
		if body == "(empty page)" || len(body) < 50 {
			t.Logf("skipped %s: body too short (%d bytes)", u, len(body))
			continue
		}
		t.Logf("search -> %s -> fetched %d bytes", u, len(body))
		return
	}
	t.Fatalf("none of the first %d search URLs produced a body (last error: %v)", min(3, len(urls)), lastErr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
