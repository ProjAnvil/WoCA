package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/websearch"
)

// newWebSearchServer builds a Server with a temp settings DB. WOCA_HOME is
// redirected so the settings crypto key does not land in the user's home.
func newWebSearchServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("WOCA_HOME", t.TempDir())
	st, err := settings.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := httptest.NewServer(New(Config{
		Static:   fstest.MapFS{},
		Logger:   zerolog.Nop(),
		Settings: st,
	}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getWebSearch(t *testing.T, ts *httptest.Server) websearchResponse {
	t.Helper()
	res, err := http.Get(ts.URL + "/api/websearch")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/websearch = %d", res.StatusCode)
	}
	var out websearchResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func putWebSearch(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/websearch", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGetWebSearchDefaultShowsEffectiveChain(t *testing.T) {
	ts := newWebSearchServer(t)
	got := getWebSearch(t, ts)

	// Nothing configured yet, but the UI must still show the two default
	// backends so the user can reorder or disable them.
	if len(got.Enabled) != 2 || got.Enabled[0] != "ddg-html" || got.Enabled[1] != "ddg-lite" {
		t.Fatalf("Enabled = %v, want [ddg-html ddg-lite]", got.Enabled)
	}
	if len(got.Providers) == 0 {
		t.Fatal("providers catalog is empty")
	}
	var ddg *websearchProviderView
	for i := range got.Providers {
		if got.Providers[i].ID == "ddg-html" {
			ddg = &got.Providers[i]
		}
	}
	if ddg == nil {
		t.Fatal("catalog is missing ddg-html")
	}
	if !ddg.Enabled || ddg.Order != 1 {
		t.Errorf("ddg-html enabled=%v order=%d, want true/1", ddg.Enabled, ddg.Order)
	}
	if got.CustomKeySet {
		t.Error("custom_key_set should be false with no stored key")
	}
}

func TestPutWebSearchPersists(t *testing.T) {
	ts := newWebSearchServer(t)
	res := putWebSearch(t, ts, `{
		"providers": ["searxng","ddg-html"],
		"searxng_endpoint": "https://searx.example.com",
		"custom_endpoint": "https://search.example/api",
		"allowed_domains": ["go.dev"],
		"denied_domains": ["spam.example"]
	}`)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/websearch = %d", res.StatusCode)
	}

	got := getWebSearch(t, ts)
	if len(got.Enabled) != 2 || got.Enabled[0] != "searxng" {
		t.Fatalf("Enabled = %v, want [searxng ddg-html]", got.Enabled)
	}
	if got.SearXNGEndpoint != "https://searxng.example.com" && got.SearXNGEndpoint != "https://searx.example.com" {
		t.Errorf("searxng_endpoint = %q", got.SearXNGEndpoint)
	}
	if got.CustomEndpoint != "https://search.example/api" {
		t.Errorf("custom_endpoint = %q", got.CustomEndpoint)
	}
	if len(got.AllowedDomains) != 1 || got.AllowedDomains[0] != "go.dev" {
		t.Errorf("allowed_domains = %v", got.AllowedDomains)
	}
	if len(got.DeniedDomains) != 1 || got.DeniedDomains[0] != "spam.example" {
		t.Errorf("denied_domains = %v", got.DeniedDomains)
	}
}

func TestPutWebSearchRejectsUnknownProvider(t *testing.T) {
	ts := newWebSearchServer(t)
	res := putWebSearch(t, ts, `{"providers":["brave"]}`)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT with an unknown provider = %d, want 400", res.StatusCode)
	}
}

func TestPutWebSearchRejectsDuplicateProvider(t *testing.T) {
	ts := newWebSearchServer(t)
	res := putWebSearch(t, ts, `{"providers":["ddg-html","ddg-html"]}`)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT with a duplicate provider = %d, want 400", res.StatusCode)
	}
}

func TestPutWebSearchRejectsBadEndpoint(t *testing.T) {
	ts := newWebSearchServer(t)
	res := putWebSearch(t, ts, `{"searxng_endpoint":"not-a-url"}`)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT with a bad endpoint = %d, want 400", res.StatusCode)
	}
}

// TestCustomKeyIsWriteOnly: the stored key must never come back over the API.
func TestCustomKeyIsWriteOnly(t *testing.T) {
	ts := newWebSearchServer(t)
	res := putWebSearch(t, ts, `{"custom_endpoint":"https://api.example/s","custom_key":"sk-secret"}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", res.StatusCode)
	}

	got := getWebSearch(t, ts)
	if !got.CustomKeySet {
		t.Fatal("custom_key_set should be true after storing a key")
	}
	body, err := http.Get(ts.URL + "/api/websearch")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Body.Close() }()
	buf := make([]byte, 4096)
	n, _ := body.Body.Read(buf)
	if strings.Contains(string(buf[:n]), "sk-secret") {
		t.Fatal("the stored key leaked into the GET response")
	}
}

// TestPutWebSearchRebuildsTheLiveChain closes the loop: an HTTP save must
// reach the chain the WebSearch tool actually uses, not just the database.
// Without this the settings page could persist correctly and still leave the
// agent searching with the old configuration.
func TestPutWebSearchRebuildsTheLiveChain(t *testing.T) {
	ts := newWebSearchServer(t)
	t.Cleanup(func() { tools.SetWebSearchConfig(websearch.Config{}) })

	res := putWebSearch(t, ts, `{
		"providers": ["searxng"],
		"searxng_endpoint": "https://searx.example.com"
	}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", res.StatusCode)
	}

	providers := tools.WebSearchRouter().Providers()
	if len(providers) != 1 {
		t.Fatalf("live chain = %d providers, want 1", len(providers))
	}
	if providers[0].ID() != "searxng" {
		t.Fatalf("live chain provider = %q, want searxng", providers[0].ID())
	}
}

// TestWebSearchTestEndpointReportsNoBackend: with a chain that has nothing
// usable, the probe must report a failure rather than pretending it worked.
func TestWebSearchTestEndpointReportsNoBackend(t *testing.T) {
	ts := newWebSearchServer(t)
	// searxng listed but unconfigured is dropped, leaving an empty chain —
	// this keeps the probe off the network.
	res := putWebSearch(t, ts, `{"providers":["searxng"]}`)
	_ = res.Body.Close()

	probe, err := http.Post(ts.URL+"/api/websearch/test", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Body.Close() }()
	var out websearchTestResponse
	if err := json.NewDecoder(probe.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.OK {
		t.Fatalf("probe with an empty chain reported ok: %+v", out)
	}
	if out.Error == "" {
		t.Error("the probe must carry the error text so the UI can show it")
	}
}
