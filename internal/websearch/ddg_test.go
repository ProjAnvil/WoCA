package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withPermissiveEndpoints lets hermetic tests point providers at loopback
// servers. Mirrors the webURLValidate seam in internal/tools.
func withPermissiveEndpoints(t *testing.T) {
	t.Helper()
	orig := endpointValidator
	endpointValidator = func(string) error { return nil }
	t.Cleanup(func() { endpointValidator = orig })
}

// ddgHTMLPage returns markup shaped like the real endpoint: one
// ".result__body" block per hit, holding the title anchor and the snippet.
func ddgHTMLPage() string {
	return `<html><body>
<div class="result results_links web-result">
  <div class="links_main result__body">
    <h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc">Go <b>Documentation</b></a></h2>
    <a class="result__snippet" href="#">The <em>official</em> docs</a>
  </div>
</div>
<div class="result results_links web-result">
  <div class="links_main result__body">
    <h2 class="result__title"><a class="result__a" href="https://example.org/plain">Plain Link</a></h2>
    <a class="result__snippet">Second snippet</a>
  </div>
</div>
</body></html>`
}

func TestDDGHTMLParsesResults(t *testing.T) {
	p := NewDDGHTML(DDGOptions{})
	got := p.parse([]byte(ddgHTMLPage()))
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	want := []Result{
		{Title: "Go Documentation", URL: "https://go.dev/doc", Snippet: "The official docs"},
		{Title: "Plain Link", URL: "https://example.org/plain", Snippet: "Second snippet"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestDDGParseFallsBackToIndexPairing: if the result container class changes,
// the parser must still pair titles and snippets by document order instead of
// returning nothing (which would read as "no results").
func TestDDGParseFallsBackToIndexPairing(t *testing.T) {
	page := `<html><body>
<a class="result__a" href="https://example.org/a">Alpha</a>
<a class="result__snippet">snip alpha</a>
<a class="result__a" href="https://example.org/b">Beta</a>
</body></html>`
	p := NewDDGHTML(DDGOptions{})
	got := p.parse([]byte(page))
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Title != "Alpha" || got[0].Snippet != "snip alpha" {
		t.Errorf("result 0 = %+v", got[0])
	}
	if got[1].Title != "Beta" || got[1].Snippet != "" {
		t.Errorf("result 1 = %+v, want empty snippet (unpaired)", got[1])
	}
}

func TestDDGLiteParsesResults(t *testing.T) {
	page := `<html><body><table>
<tr><td><a class="result-link" href="https://x/1">One</a></td></tr>
<tr><td class="result-snippet">snip one</td></tr>
<tr><td><a class="result-link" href="https://x/2">Two</a></td></tr>
<tr><td class="result-snippet">snip two</td></tr>
</table></body></html>`
	p := NewDDGLite(DDGOptions{})
	got := p.parse([]byte(page))
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Title != "One" || got[0].Snippet != "snip one" || got[0].URL != "https://x/1" {
		t.Errorf("result 0 = %+v", got[0])
	}
}

// TestDDGSearchBlockedPageIsNotNoResults is the P0 regression.
//
// DuckDuckGo answers untrusted clients with HTTP 200 and a challenge page. The
// old regex parser found no result anchors and reported "No results found",
// which tells the model the information does not exist and invites invention.
func TestDDGSearchBlockedPageIsNotNoResults(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><div class="anomaly-modal">Please complete the challenge</div></body></html>`))
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	_, err := p.Search(t.Context(), "golang", 0)
	if err == nil {
		t.Fatal("blocked page returned nil error, want ErrRateLimited")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("blocked page error = %v, want ErrRateLimited", err)
	}
	if !Failover(err) {
		t.Error("blocked page must be failover-able so the router tries the next backend")
	}
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.Provider != ddgHTMLID {
		t.Errorf("RateLimitError.Provider = %q, want %q", rl.Provider, ddgHTMLID)
	}
}

func TestDDGSearchHTTP202IsRateLimited(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	_, err := p.Search(t.Context(), "golang", 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("HTTP 202 error = %v, want ErrRateLimited", err)
	}
}

func TestDDGSearchRetryAfterPropagates(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	_, err := p.Search(t.Context(), "golang", 0)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("HTTP 429 error = %T, want *RateLimitError", err)
	}
	if rl.RetryAfter != 45*time.Second {
		t.Fatalf("RetryAfter = %v, want 45s", rl.RetryAfter)
	}
}

// TestDDGSearchServerErrorIsNotFailover: a 500 is the backend's problem, not a
// licence to silently hop to another provider.
func TestDDGSearchServerErrorIsNotFailover(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	_, err := p.Search(t.Context(), "golang", 0)
	if err == nil {
		t.Fatal("HTTP 500 = nil error")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Errorf("HTTP 500 must not be reported as rate limiting: %v", err)
	}
	if Failover(err) {
		t.Error("HTTP 500 must not trigger failover")
	}
}

// TestDDGSearchEmptyIsSuccess: a genuinely empty result set is success, not an
// error. This is the other half of the P0 distinction.
func TestDDGSearchEmptyIsSuccess(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><p>No results.</p></body></html>`))
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	got, err := p.Search(t.Context(), "zzzz", 0)
	if err != nil {
		t.Fatalf("empty result set = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0", len(got))
	}
}

func TestDDGSearchSendsQuery(t *testing.T) {
	withPermissiveEndpoints(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Write([]byte(ddgHTMLPage()))
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	if _, err := p.Search(t.Context(), "go mcp server", 0); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "go mcp server" {
		t.Fatalf("q = %q, want %q", gotQuery, "go mcp server")
	}
}

func TestDDGSearchClampsLimit(t *testing.T) {
	withPermissiveEndpoints(t)
	var b strings.Builder
	for i := range 25 {
		b.WriteString(`<div class="result__body"><a class="result__a" href="https://e.org/`)
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(`">T</a><a class="result__snippet">S</a></div>`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})

	got, err := p.Search(t.Context(), "x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("limit 3 gave %d results", len(got))
	}

	// Zero means "use the default", and the hard cap applies to oversized asks.
	got, _ = p.Search(t.Context(), "x", 0)
	if len(got) != defaultMaxResults {
		t.Fatalf("limit 0 gave %d results, want default %d", len(got), defaultMaxResults)
	}
	got, _ = p.Search(t.Context(), "x", 1000)
	if len(got) != maxResultsHardCap {
		t.Fatalf("limit 1000 gave %d results, want cap %d", len(got), maxResultsHardCap)
	}
}

func TestDDGProviderIdentity(t *testing.T) {
	withPermissiveEndpoints(t)
	html := NewDDGHTML(DDGOptions{})
	lite := NewDDGLite(DDGOptions{})
	if html.ID() != ddgHTMLID || lite.ID() != ddgLiteID {
		t.Fatalf("ids = %q / %q", html.ID(), lite.ID())
	}
	if !html.Available() || !lite.Available() {
		t.Fatal("default DDG endpoints should be available without configuration")
	}
	// An empty Endpoint means "use the built-in default", not "disabled".
	// Disabling a provider is a router-level decision (omit it from the chain),
	// so construction must always produce a usable provider.
	if !NewDDGHTML(DDGOptions{Endpoint: ""}).Available() {
		t.Error("empty Endpoint should fall back to the built-in default")
	}
}

// TestDDGEndpointIsSSRFGated: a configured endpoint pointing at a metadata
// service must be rejected, not dialled.
func TestDDGEndpointIsSSRFGated(t *testing.T) {
	if NewDDGHTML(DDGOptions{Endpoint: "http://169.254.169.254/latest/meta-data/"}).Available() {
		t.Fatal("endpoint validator must reject link-local metadata addresses")
	}
	if NewDDGHTML(DDGOptions{Endpoint: "http://127.0.0.1:1/"}).Available() {
		t.Fatal("endpoint validator must reject loopback")
	}
}

func TestDDGSearchContextCancelled(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(ddgHTMLPage()))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := NewDDGHTML(DDGOptions{Endpoint: srv.URL + "/"})
	if _, err := p.Search(ctx, "golang", 0); err == nil {
		t.Fatal("cancelled context = nil error")
	}
}

func TestUnwrapDDGURL(t *testing.T) {
	cases := []struct{ href, want string }{
		{"https://example.com/page", "https://example.com/page"},
		{"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc", "https://go.dev/doc"},
		// uddg=%25zz unescapes once to "%zz"; the second unescape fails, so
		// the once-unescaped value is what we can offer.
		{"//duckduckgo.com/l/?uddg=%25zz", "%zz"},
		// A malformed escape makes url.Query drop the pair entirely, so there
		// is nothing to unwrap and the original is kept.
		{"//duckduckgo.com/l/?uddg=not%zz-escapable", "//duckduckgo.com/l/?uddg=not%zz-escapable"},
		{"ht tp://bad url", "ht tp://bad url"},
	}
	for _, c := range cases {
		if got := unwrapDDGURL(c.href); got != c.want {
			t.Errorf("unwrapDDGURL(%q) = %q, want %q", c.href, got, c.want)
		}
	}
}

func TestCollapseSpace(t *testing.T) {
	if got := collapseSpace("  a\n\t b   c  "); got != "a b c" {
		t.Fatalf("collapseSpace = %q", got)
	}
	if got := collapseSpace(""); got != "" {
		t.Fatalf("collapseSpace(\"\") = %q", got)
	}
}
