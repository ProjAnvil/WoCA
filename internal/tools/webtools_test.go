package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	got, err := fetchHTTP(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "<html><body>hello web</body></html>" {
		t.Fatalf("fetchHTTP body = %q", got)
	}
	if got, err := fetchHTTP(context.Background(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("non-200 = %q, %v; want HTTP 404 error", got, err)
	}
}

func TestFetchHTTPInvalidURL(t *testing.T) {
	withPermissiveWebURL(t)
	// A control character makes http.NewRequest fail before any dialing.
	if _, err := fetchHTTP(context.Background(), "http://127.0.0.1:1/\x7f"); err == nil {
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
	if _, err := fetchHTTP(context.Background(), srv.URL); err == nil {
		t.Fatal("fetchHTTP(truncated body) = nil error, want read failure")
	}
}

func TestFetchHTTPConnectionRefused(t *testing.T) {
	withPermissiveWebURL(t)
	// Port 1 on loopback is closed; the dial fails without leaving the host.
	if _, err := fetchHTTP(context.Background(), "http://127.0.0.1:1/"); err == nil {
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
	res, err := tool.Invoke(context.Background(), map[string]any{"url": srv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Some fancy text") {
		t.Fatalf("WebFetch page = %q", res.Content)
	}

	res, err = tool.Invoke(context.Background(), map[string]any{"url": srv.URL + "/empty"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "(empty page)" {
		t.Fatalf("WebFetch empty = %q", res.Content)
	}

	// Fetch failures surface as tool errors: with the real validator restored
	// mid-test, the loopback test server is SSRF-blocked before any request.
	webURLValidate = orig
	if _, err := tool.Invoke(context.Background(), map[string]any{"url": srv.URL}); err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("WebFetch blocked = %v, want fetch error", err)
	}
}

func TestWebSearchToolViaTestServer(t *testing.T) {
	withPermissiveWebURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "golang" {
			w.Write([]byte("<html>no anchors at all</html>"))
			return
		}
		w.Write([]byte(`<html><body>
<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc">Go <b>Documentation</b></a>
<a class="result__snippet" href="#">The <em>official</em> docs</a>
<a class="result__a" href="https://example.org/plain">Plain Link</a>
</body></html>`))
	}))
	defer srv.Close()

	orig := webSearchEndpoint
	webSearchEndpoint = srv.URL + "/"
	t.Cleanup(func() { webSearchEndpoint = orig })

	reg := NewRegistry(t.TempDir())
	tool, ok := reg.Get("WebSearch")
	if !ok {
		t.Fatal("WebSearch tool missing")
	}
	res, err := tool.Invoke(context.Background(), map[string]any{"query": "golang"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go Documentation", "https://go.dev/doc", "The official docs", "Plain Link", "https://example.org/plain"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("WebSearch result missing %q:\n%s", want, res.Content)
		}
	}

	res, err = tool.Invoke(context.Background(), map[string]any{"query": "no-results-query"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "No results found." {
		t.Fatalf("WebSearch no results = %q", res.Content)
	}
}

func TestWebSearchToolFetchError(t *testing.T) {
	withPermissiveWebURL(t)
	orig := webSearchEndpoint
	webSearchEndpoint = "http://127.0.0.1:1/"
	t.Cleanup(func() { webSearchEndpoint = orig })

	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("WebSearch")
	if _, err := tool.Invoke(context.Background(), map[string]any{"query": "x"}); err == nil || !strings.Contains(err.Error(), `search "x"`) {
		t.Fatalf("WebSearch fetch error = %v, want search error wrapping", err)
	}
}

func TestParseDuckDuckGoLimitsResults(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(`<a class="result__a" href="https://example.com/` + string(rune('a'+i)) + `">Result ` + string(rune('0'+i)) + `</a>`)
		if i > 4 { // results 6+ have no matching snippet entries
			continue
		}
		b.WriteString(`<a class="result__snippet">snip ` + string(rune('0'+i)) + `</a>`)
	}
	out := parseDuckDuckGo(b.String())
	if strings.Contains(out, "Result 9") || strings.Contains(out, "Result 8") {
		t.Fatalf("more than 8 results kept:\n%s", out)
	}
	if !strings.Contains(out, "Result 0") || !strings.Contains(out, "snip 0") {
		t.Fatalf("first result/snippet missing:\n%s", out)
	}
	if got := strings.Count(out, "\n   https://example.com/"); got != 8 {
		t.Fatalf("got %d result URLs, want 8:\n%s", got, out)
	}
}

func TestParseDuckDuckGoEmpty(t *testing.T) {
	if out := parseDuckDuckGo("<html>nothing</html>"); out != "" {
		t.Fatalf("parseDuckDuckGo empty = %q", out)
	}
}

func TestCleanDuckDuckGoURL(t *testing.T) {
	cases := []struct {
		href, want string
	}{
		{"https://example.com/page", "https://example.com/page"},
		{"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc", "https://go.dev/doc"},
		{"//duckduckgo.com/l/?uddg=not%zz-escapable", "//duckduckgo.com/l/?uddg=not%zz-escapable"}, // invalid escape: dropped, href kept
		{"ht tp://bad url", "ht tp://bad url"},                                                     // url.Parse failure: passthrough
	}
	for _, c := range cases {
		if got := cleanDuckDuckGoURL(c.href); got != c.want {
			t.Errorf("cleanDuckDuckGoURL(%q) = %q, want %q", c.href, got, c.want)
		}
	}
}

func TestWebSearchToolWithBackend(t *testing.T) {
	tool := WebSearchToolWithBackend(func(ctx context.Context, query string) (string, error) {
		if query == "boom" {
			return "", context.DeadlineExceeded
		}
		if query == "empty" {
			return "", nil
		}
		return "1. result\n   https://example.com", nil
	})
	res, err := tool.Invoke(context.Background(), map[string]any{"query": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "1. result\n   https://example.com" {
		t.Fatalf("backend result = %q", res.Content)
	}
	res, err = tool.Invoke(context.Background(), map[string]any{"query": "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "No results found." {
		t.Fatalf("empty backend = %q", res.Content)
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"query": "boom"}); err == nil ||
		!strings.Contains(err.Error(), `search "boom"`) || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("backend error = %v, want wrapped search error", err)
	}
}
