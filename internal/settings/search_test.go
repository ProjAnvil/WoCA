package settings

import (
	"testing"

	"github.com/projanvil/woca/internal/websearch"
)

func TestSearchConfigMapsSettings(t *testing.T) {
	s := Settings{
		SearchProviders:       []string{"searxng", "ddg-html"},
		SearchSearXNGEndpoint: "https://searx.example.com",
		SearchCustomEndpoint:  "https://search.example/api",
		SearchAllowedDomains:  []string{"go.dev"},
		SearchDeniedDomains:   []string{"spam.example"},
	}
	cfg := s.SearchConfig("sk-live")

	if len(cfg.Order) != 2 || cfg.Order[0] != "searxng" {
		t.Errorf("Order = %v", cfg.Order)
	}
	if cfg.SearXNG != "https://searx.example.com" {
		t.Errorf("SearXNG = %q", cfg.SearXNG)
	}
	if cfg.Custom != "https://search.example/api" {
		t.Errorf("Custom = %q", cfg.Custom)
	}
	if cfg.CustomKey != "sk-live" {
		t.Errorf("CustomKey = %q", cfg.CustomKey)
	}
	if len(cfg.AllowedDomains) != 1 || cfg.AllowedDomains[0] != "go.dev" {
		t.Errorf("AllowedDomains = %v", cfg.AllowedDomains)
	}
	if len(cfg.DeniedDomains) != 1 || cfg.DeniedDomains[0] != "spam.example" {
		t.Errorf("DeniedDomains = %v", cfg.DeniedDomains)
	}
}

func TestSearchConfigEmptyGivesDefaultChain(t *testing.T) {
	// No settings rows at all must still produce a working default chain.
	cfg := Settings{}.SearchConfig("")
	ids := make([]string, 0, len(cfg.Order))
	for _, p := range cfg.Build() {
		ids = append(ids, p.ID())
	}
	if len(ids) != 2 {
		t.Fatalf("default chain = %v, want the two DuckDuckGo endpoints", ids)
	}
}

func TestSearchConfigWithOverrideNoopWhenUnset(t *testing.T) {
	cfg := Settings{SearchProviders: []string{"ddg-html"}}.SearchConfig("")
	out := SearchConfigWithOverride(cfg, "", "")
	if len(out.Order) != 1 || out.Order[0] != "ddg-html" {
		t.Fatalf("Order = %v, want it untouched", out.Order)
	}
}

// TestSearchConfigWithOverrideAppendsCustom: an explicit order that omits
// "custom" would silently ignore --search-api, so the override has to add it.
func TestSearchConfigWithOverrideAppendsCustom(t *testing.T) {
	cfg := Settings{SearchProviders: []string{"searxng"}, SearchSearXNGEndpoint: "https://searx.example.com"}.SearchConfig("")
	out := SearchConfigWithOverride(cfg, "https://api.example/s", "sk")

	if out.Custom != "https://api.example/s" || out.CustomKey != "sk" {
		t.Fatalf("Custom = %q / CustomKey = %q", out.Custom, out.CustomKey)
	}
	if len(out.Order) != 2 || out.Order[1] != websearch.ProviderCustom {
		t.Fatalf("Order = %v, want custom appended", out.Order)
	}
}

func TestSearchConfigWithOverrideIdempotent(t *testing.T) {
	cfg := Settings{SearchProviders: []string{websearch.ProviderCustom}}.SearchConfig("")
	out := SearchConfigWithOverride(cfg, "https://api.example/s", "")
	if len(out.Order) != 1 {
		t.Fatalf("Order = %v, want custom listed once", out.Order)
	}
}

// TestSearchConfigRoundTripsThroughSettings: what the settings page writes is
// what the search chain uses — the point of the whole exercise.
func TestSearchConfigRoundTripsThroughSettings(t *testing.T) {
	st := openTest(t)
	cfg := Default()
	cfg.SearchProviders = []string{"tavily"}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got := st.Load().SearchConfig("")
	if len(got.Order) != 1 || got.Order[0] != "tavily" {
		t.Fatalf("Order = %v, want [tavily]", got.Order)
	}
	built := got.Build()
	if len(built) != 1 || built[0].ID() != "tavily" {
		t.Fatalf("chain = %v, want only tavily", built)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}
