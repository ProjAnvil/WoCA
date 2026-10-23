package configsvc

// The web-search domain on the config plane: the console pins an enterprise
// default chain that rides the pack down to every managed machine. It shares
// the settings GET/PUT surface rather than getting its own table — the
// provider list expresses enable/disable, so a non-empty list pins the exact
// allowed set without needing a bool that the overlay cannot lock.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

func TestWebSearchRoundTrip(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", "", map[string]any{
		"search_providers":        []string{"searxng", "ddg-html"},
		"search_searxng_endpoint": "https://searx.internal.example.com",
		"search_custom_endpoint":  "https://search.internal/api",
		"search_custom_key":       "sk-managed",
		"search_allowed_domains":  []string{"go.dev"},
		"search_denied_domains":   []string{"spam.example"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	cfg := f.st.Load()
	if len(cfg.SearchProviders) != 2 || cfg.SearchProviders[0] != "searxng" {
		t.Fatalf("store providers = %v, want [searxng ddg-html]", cfg.SearchProviders)
	}
	if cfg.SearchSearXNGEndpoint != "https://searx.internal.example.com" {
		t.Errorf("store searxng endpoint = %q", cfg.SearchSearXNGEndpoint)
	}
	if cfg.SearchCustomEndpoint != "https://search.internal/api" {
		t.Errorf("store custom endpoint = %q", cfg.SearchCustomEndpoint)
	}
	if got := f.st.GetSecret(settings.SecretSearchCustomKey); got != "sk-managed" {
		t.Errorf("stored custom key = %q, want sk-managed", got)
	}

	// GET reflects the configuration and reports the key without exposing it.
	get := f.do("GET", "/api/settings", "", nil)
	defer get.Body.Close()
	var out struct {
		Providers      []string `json:"search_providers"`
		SearXNG        string   `json:"search_searxng_endpoint"`
		Custom         string   `json:"search_custom_endpoint"`
		CustomKeySet   bool     `json:"search_custom_key_set"`
		AllowedDomains []string `json:"search_allowed_domains"`
		DeniedDomains  []string `json:"search_denied_domains"`
	}
	if err := json.NewDecoder(get.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 2 || out.Providers[0] != "searxng" {
		t.Errorf("GET providers = %v", out.Providers)
	}
	if !out.CustomKeySet {
		t.Error("GET should report that a custom key is stored")
	}
	if len(out.AllowedDomains) != 1 || out.AllowedDomains[0] != "go.dev" {
		t.Errorf("GET allowed domains = %v", out.AllowedDomains)
	}
	if len(out.DeniedDomains) != 1 || out.DeniedDomains[0] != "spam.example" {
		t.Errorf("GET denied domains = %v", out.DeniedDomains)
	}

	// The key itself must not appear anywhere in the payload.
	raw := f.do("GET", "/api/settings", "", nil)
	defer raw.Body.Close()
	buf := make([]byte, 8192)
	n, _ := raw.Body.Read(buf)
	if contains(string(buf[:n]), "sk-managed") {
		t.Fatal("the stored custom key leaked into the GET payload")
	}
}

// TestWebSearchValidation: the console is the management plane, so a bad
// value must be refused rather than shipped to every machine.
func TestWebSearchValidation(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []map[string]any{
		{"search_providers": []string{"brave"}},
		{"search_providers": []string{"ddg-html", "ddg-html"}},
		{"search_searxng_endpoint": "not-a-url"},
	} {
		resp := f.do("PUT", "/api/settings", "", bad)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %v = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestWebSearchAbsentLeavesUserChoice: removing every search field from the
// payload must not silently clear a user's own configuration.
func TestWebSearchAbsentLeavesUserChoice(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", "", map[string]any{
		"search_providers": []string{"tavily"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	// A later update that does not mention search at all.
	other := f.do("PUT", "/api/settings", "", map[string]any{"output_style": "concise"})
	defer other.Body.Close()
	if other.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", other.StatusCode)
	}
	if got := f.st.Load().SearchProviders; len(got) != 1 || got[0] != "tavily" {
		t.Fatalf("providers = %v, want the previous value preserved", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
