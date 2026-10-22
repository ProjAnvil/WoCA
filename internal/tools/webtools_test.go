package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/websearch"
)

// withPermissiveWebURL swaps the SSRF validator so hermetic httptest servers
// (loopback) are reachable, restoring the real validator afterwards. Tests
// using it must not run in parallel.
func withPermissiveWebURL(t *testing.T) {
	t.Helper()
	orig := webURLValidate
	webURLValidate = func(string) error { return nil }
	t.Cleanup(func() { webURLValidate = orig })
}

func TestFetchHTTPSuccessAndHTTPStatus(t *testing.T) {
	withPermissiveWebURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("<html><body>hello web</body></html>"))
	}))
	defer srv.Close()

	got, err := fetchHTTP(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "<html><body>hello web</body></html>" {
		t.Fatalf("fetchHTTP body = %q", got)
	}
	if got, err := fetchHTTP(t.Context(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("non-200 = %q, %v; want HTTP 404 error", got, err)
	}
}

func TestFetchHTTPInvalidURL(t *testing.T) {
	withPermissiveWebURL(t)
	// A control character makes http.NewRequest fail before any dialing.
	if _, err := fetchHTTP(t.Context(), "http://127.0.0.1:1/\x7f"); err == nil {
		t.Fatal("fetchHTTP(invalid URL) = nil error")
	}
}

func TestFetchHTTPTruncatedBody(t *testing.T) {
	withPermissiveWebURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("short")) // fewer bytes than declared: read fails mid-body
	}))
	defer srv.Close()
	if _, err := fetchHTTP(t.Context(), srv.URL); err == nil {
		t.Fatal("fetchHTTP(truncated body) = nil error, want read failure")
	}
}

func TestFetchHTTPConnectionRefused(t *testing.T) {
	withPermissiveWebURL(t)
	// Port 1 on loopback is closed; the dial fails without leaving the host.
	if _, err := fetchHTTP(t.Context(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("fetchHTTP to closed port = nil error, want connection failure")
	}
}

func TestStripHTML(t *testing.T) {
	if got := stripHTML("<script>var x=1;</script><style>a{}</style><p>Hi <b>there</b></p>"); got != "Hi there" {
		t.Fatalf("stripHTML tags = %q", got)
	}
	if got := stripHTML("  a\t\n b   c  "); got != "a b c" {
		t.Fatalf("stripHTML whitespace = %q", got)
	}
	if got := stripHTML(""); got != "" {
		t.Fatalf("stripHTML empty = %q", got)
	}
	long := strings.Repeat("x", maxWebChars+500)
	got := stripHTML(long)
	if len(got) != maxWebChars+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated length = %d, suffix = %q", len(got), got[len(got)-10:])
	}
}

func TestWebFetchTool(t *testing.T) {
	orig := webURLValidate
	withPermissiveWebURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Write([]byte("<html><head><title>T</title></head><body><p>Some <em>fancy</em> text</p></body></html>"))
		case "/empty":
			w.Write([]byte("<html><body>   </body></html>"))
		}
	}))
	defer srv.Close()

	reg := NewRegistry(t.TempDir())
	tool, ok := reg.Get("WebFetch")
	if !ok {
		t.Fatal("WebFetch tool missing")
	}
	res, err := tool.Invoke(t.Context(), map[string]any{"url": srv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Some fancy text") {
		t.Fatalf("WebFetch page = %q", res.Content)
	}

	res, err = tool.Invoke(t.Context(), map[string]any{"url": srv.URL + "/empty"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "(empty page)" {
		t.Fatalf("WebFetch empty = %q", res.Content)
	}

	// Fetch failures surface as tool errors: with the real validator restored
	// mid-test, the loopback test server is SSRF-blocked before any request.
	webURLValidate = orig
	if _, err := tool.Invoke(t.Context(), map[string]any{"url": srv.URL}); err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("WebFetch blocked = %v, want fetch error", err)
	}
}

// ---------- WebSearch over the provider router ----------

// stubProvider is a scripted websearch.Provider for tool-layer tests. The real
// providers are exercised in internal/websearch.
type stubProvider struct {
	id      string
	results []websearch.Result
	err     error
	calls   int
}

func (s *stubProvider) ID() string      { return s.id }
func (s *stubProvider) Name() string    { return s.id }
func (s *stubProvider) Available() bool { return true }

func (s *stubProvider) Search(_ context.Context, _ string, _ int) ([]websearch.Result, error) {
	s.calls++
	return s.results, s.err
}

func TestWebSearchToolRendersResults(t *testing.T) {
	p := &stubProvider{id: "stub", results: []websearch.Result{
		{Title: "Go Documentation", URL: "https://go.dev/doc", Snippet: "The official docs"},
	}}
	tool := newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{
		Providers:   []websearch.Provider{p},
		MinInterval: time.Millisecond,
	}))
	res, err := tool.Invoke(t.Context(), map[string]any{"query": "golang"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go Documentation", "https://go.dev/doc", "The official docs"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("output missing %q:\n%s", want, res.Content)
		}
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
}

// TestWebSearchToolEmptyIsNotAnError: a search that genuinely finds nothing is
// a success, and the model must be told so in words it cannot confuse with a
// failure.
func TestWebSearchToolEmptyIsNotAnError(t *testing.T) {
	tool := newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{
		Providers:   []websearch.Provider{&stubProvider{id: "stub"}},
		MinInterval: time.Millisecond,
	}))
	res, err := tool.Invoke(t.Context(), map[string]any{"query": "zzzz"})
	if err != nil {
		t.Fatalf("empty result set = %v, want nil error", err)
	}
	if res.Content != noResultsText {
		t.Fatalf("content = %q, want %q", res.Content, noResultsText)
	}
}

// TestWebSearchToolRateLimitIsNeverNoResults is the regression guard for the
// bug this refactor fixes: a throttled backend used to render as an empty
// result set, which makes models conclude the information does not exist.
func TestWebSearchToolRateLimitIsNeverNoResults(t *testing.T) {
	tool := newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{
		Providers:   []websearch.Provider{&stubProvider{id: "stub", err: &websearch.RateLimitError{Provider: "stub"}}},
		MinInterval: time.Millisecond,
	}))
	_, err := tool.Invoke(t.Context(), map[string]any{"query": "golang"})
	if err == nil {
		t.Fatal("rate-limited search = nil error")
	}
	if strings.Contains(err.Error(), noResultsText) {
		t.Fatalf("throttling reported as no-results: %v", err)
	}
	if !strings.Contains(err.Error(), "do not retry or rephrase") {
		t.Fatalf("error = %v, want the actionable rate-limit wording", err)
	}
}

func TestWebSearchToolNoBackend(t *testing.T) {
	tool := newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{MinInterval: time.Millisecond}))
	_, err := tool.Invoke(t.Context(), map[string]any{"query": "golang"})
	if err == nil {
		t.Fatal("router with no providers = nil error")
	}
	if !strings.Contains(err.Error(), "no search backend") {
		t.Fatalf("error = %v, want it to say no backend is configured", err)
	}
}

// TestWebSearchToolWithProvidersIsExclusive: --search-api means "use this and
// nothing else", so a hard failure must not be papered over by another backend.
func TestWebSearchToolWithProvidersIsExclusive(t *testing.T) {
	failing := &stubProvider{id: "configured", err: context.DeadlineExceeded}
	tool := WebSearchToolWithProviders(failing)
	if _, err := tool.Invoke(t.Context(), map[string]any{"query": "golang"}); err == nil {
		t.Fatal("configured backend failure = nil error")
	}
	if failing.calls != 1 {
		t.Fatalf("configured provider calls = %d, want 1", failing.calls)
	}
}

func TestWebSearchToolFailsOverToSecondProvider(t *testing.T) {
	first := &stubProvider{id: "first", err: &websearch.RateLimitError{Provider: "first"}}
	second := &stubProvider{id: "second", results: []websearch.Result{
		{Title: "From second", URL: "https://example.org"},
	}}
	tool := WebSearchToolWithProviders(first, second)
	res, err := tool.Invoke(t.Context(), map[string]any{"query": "golang"})
	if err != nil {
		t.Fatalf("failover failed: %v", err)
	}
	if !strings.Contains(res.Content, "From second") {
		t.Fatalf("output = %q, want the second provider's result", res.Content)
	}
}

func TestWebSearchToolWrapsQueryInError(t *testing.T) {
	tool := newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{
		Providers:   []websearch.Provider{&stubProvider{id: "stub", err: context.DeadlineExceeded}},
		MinInterval: time.Millisecond,
	}))
	_, err := tool.Invoke(t.Context(), map[string]any{"query": "go mcp"})
	if err == nil || !strings.Contains(err.Error(), `search "go mcp"`) {
		t.Fatalf("error = %v, want it to name the query", err)
	}
}
