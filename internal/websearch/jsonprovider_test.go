package websearch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- SearXNG ----------

func TestSearXNGParsesResults(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q, want json", r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"query":"go","results":[
			{"url":"https://go.dev/doc","title":"Go Docs","content":"The official docs","publishedDate":"2026-01-02","score":0.9},
			{"url":"https://example.org","title":"Example","content":"second"}
		]}`)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search"})
	got, err := p.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Title != "Go Docs" || got[0].URL != "https://go.dev/doc" || got[0].Snippet != "The official docs" {
		t.Errorf("result 0 = %+v", got[0])
	}
	if got[0].Published == nil {
		t.Error("publishedDate should be parsed when present")
	} else if got[0].Published.Format("2006-01-02") != "2026-01-02" {
		t.Errorf("Published = %v", got[0].Published)
	}
	if got[0].Score != 0.9 {
		t.Errorf("Score = %v, want 0.9", got[0].Score)
	}
	// Missing publishedDate/score must stay zero-valued, not error.
	if got[1].Published != nil || got[1].Score != 0 {
		t.Errorf("result 1 optional fields = %v / %v, want nil / 0", got[1].Published, got[1].Score)
	}
}

func TestSearXNGSupportsQueryPlaceholder(t *testing.T) {
	withPermissiveEndpoints(t)
	var gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search?q={q}&format=json"})
	if _, err := p.Search(t.Context(), "hello world", 0); err != nil {
		t.Fatal(err)
	}
	if gotQ != "hello world" {
		t.Fatalf("q = %q, want %q", gotQ, "hello world")
	}
}

// TestSearXNG403ExplainsItself: many public instances disable format=json and
// answer 403. A bare "HTTP 403" leaves the operator guessing, so the message
// has to name the cause.
func TestSearXNG403ExplainsItself(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search"})
	_, err := p.Search(t.Context(), "go", 0)
	if err == nil {
		t.Fatal("403 = nil error")
	}
	if !strings.Contains(err.Error(), "format=json") {
		t.Errorf("403 error = %q, want it to mention format=json", err)
	}
	if Failover(err) {
		t.Error("403 must not trigger failover; it is a configuration problem")
	}
}

func TestSearXNGRateLimited(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search"})
	_, err := p.Search(t.Context(), "go", 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 = %v, want ErrRateLimited", err)
	}
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %v, want 120s", rl.RetryAfter)
	}
}

func TestSearXNGEmptyIsSuccess(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search"})
	got, err := p.Search(t.Context(), "zzzz", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty = %d results, %v; want 0, nil", len(got), err)
	}
}

func TestSearXNGMalformedJSON(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<html>not json</html>`)
	}))
	defer srv.Close()

	p := NewSearXNG(SearXNGOptions{Endpoint: srv.URL + "/search"})
	if _, err := p.Search(t.Context(), "go", 0); err == nil {
		t.Fatal("malformed JSON = nil error")
	}
}

func TestSearXNGRequiresEndpoint(t *testing.T) {
	if NewSearXNG(SearXNGOptions{}).Available() {
		t.Fatal("SearXNG without an endpoint must be unavailable")
	}
	// Checked before the permissive swap: a configured endpoint pointing at a
	// metadata service must be rejected, not dialled.
	if NewSearXNG(SearXNGOptions{Endpoint: "http://169.254.169.254/"}).Available() {
		t.Fatal("SearXNG endpoint must pass the SSRF policy")
	}
	withPermissiveEndpoints(t)
	if !NewSearXNG(SearXNGOptions{Endpoint: "https://searx.example.com"}).Available() {
		t.Fatal("SearXNG with an endpoint should be available")
	}
}

// ---------- Tavily (keyless) ----------

func TestTavilyKeylessRequestShape(t *testing.T) {
	withPermissiveEndpoints(t)
	var (
		gotMode, gotMethod, gotUA string
		body                      map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMode = r.Header.Get("X-Tavily-Access-Mode")
		gotMethod = r.Method
		gotUA = r.Header.Get("User-Agent")
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[{"title":"T","url":"https://e.org","content":"C","score":0.7}]}`)
	}))
	defer srv.Close()

	p := NewTavily(TavilyOptions{Endpoint: srv.URL + "/search"})
	got, err := p.Search(t.Context(), "go mcp", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotMode != "keyless" {
		t.Errorf("X-Tavily-Access-Mode = %q, want keyless", gotMode)
	}
	if gotUA == "" {
		t.Error("User-Agent must be set")
	}
	if body["query"] != "go mcp" {
		t.Errorf("body query = %v", body["query"])
	}
	if body["max_results"] != float64(5) {
		t.Errorf("body max_results = %v, want 5", body["max_results"])
	}
	if body["search_depth"] != "basic" {
		t.Errorf("body search_depth = %v, want basic", body["search_depth"])
	}
	// Keyless mode must never send an Authorization header.
	if len(got) != 1 || got[0].Title != "T" {
		t.Fatalf("results = %+v", got)
	}
	if got[0].Snippet != "C" {
		t.Errorf("Snippet = %q, want content field", got[0].Snippet)
	}
	if got[0].Content != "C" {
		t.Errorf("Content = %q, want populated (backend already paid for it)", got[0].Content)
	}
	if got[0].Score != 0.7 {
		t.Errorf("Score = %v, want 0.7", got[0].Score)
	}
}

func TestTavilyRateLimited(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewTavily(TavilyOptions{Endpoint: srv.URL + "/search"})
	_, err := p.Search(t.Context(), "go", 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 = %v, want ErrRateLimited", err)
	}
	if !Failover(err) {
		t.Error("429 must be failover-able")
	}
}

func TestTavilyServerErrorNotFailover(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := NewTavily(TavilyOptions{Endpoint: srv.URL + "/search"})
	_, err := p.Search(t.Context(), "go", 0)
	if err == nil || Failover(err) {
		t.Fatalf("502 = %v; want an error that does not trigger failover", err)
	}
}

func TestTavilyDefaultLimit(t *testing.T) {
	withPermissiveEndpoints(t)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	p := NewTavily(TavilyOptions{Endpoint: srv.URL + "/search"})
	if _, err := p.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if body["max_results"] != float64(defaultMaxResults) {
		t.Errorf("max_results = %v, want default %d", body["max_results"], defaultMaxResults)
	}
}

func TestTavilyIdentity(t *testing.T) {
	p := NewTavily(TavilyOptions{})
	if p.ID() != tavilyID {
		t.Fatalf("ID = %q, want %q", p.ID(), tavilyID)
	}
	if p.Name() == "" {
		t.Error("Name should be human-readable")
	}
}
