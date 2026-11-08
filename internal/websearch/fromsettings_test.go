package websearch

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

// openSettingsStore backs the round-trip test with a temp sqlite database and
// the crypto key under a temp HOME (mirrors internal/settings' own test
// helper; this package needs it because ConfigFromSettings consumes a
// settings.Store snapshot).
func openSettingsStore(t *testing.T) *settings.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := settings.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestConfigFromSettingsMapsSettings(t *testing.T) {
	s := settings.Settings{
		SearchProviders:       []string{"searxng", "ddg-html"},
		SearchSearXNGEndpoint: "https://searx.example.com",
		SearchCustomEndpoint:  "https://search.example/api",
		SearchAllowedDomains:  []string{"go.dev"},
		SearchDeniedDomains:   []string{"spam.example"},
	}
	cfg := ConfigFromSettings(s, "sk-live")

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

func TestConfigFromSettingsEmptyGivesDefaultChain(t *testing.T) {
	// No settings rows at all must still produce a working default chain.
	cfg := ConfigFromSettings(settings.Settings{}, "")
	ids := make([]string, 0, len(cfg.Order))
	for _, p := range cfg.Build() {
		ids = append(ids, p.ID())
	}
	if len(ids) != 2 {
		t.Fatalf("default chain = %v, want the two DuckDuckGo endpoints", ids)
	}
}

func TestWithSearchAPIOverrideNoopWhenUnset(t *testing.T) {
	cfg := ConfigFromSettings(settings.Settings{SearchProviders: []string{"ddg-html"}}, "")
	out := WithSearchAPIOverride(cfg, "", "")
	if len(out.Order) != 1 || out.Order[0] != "ddg-html" {
		t.Fatalf("Order = %v, want it untouched", out.Order)
	}
}

// TestWithSearchAPIOverrideAppendsCustom: an explicit order that omits
// "custom" would silently ignore --search-api, so the override has to add it.
func TestWithSearchAPIOverrideAppendsCustom(t *testing.T) {
	cfg := ConfigFromSettings(settings.Settings{
		SearchProviders:       []string{"searxng"},
		SearchSearXNGEndpoint: "https://searx.example.com",
	}, "")
	out := WithSearchAPIOverride(cfg, "https://api.example/s", "sk")

	if out.Custom != "https://api.example/s" || out.CustomKey != "sk" {
		t.Fatalf("Custom = %q / CustomKey = %q", out.Custom, out.CustomKey)
	}
	if len(out.Order) != 2 || out.Order[1] != ProviderCustom {
		t.Fatalf("Order = %v, want custom appended", out.Order)
	}
}

func TestWithSearchAPIOverrideIdempotent(t *testing.T) {
	cfg := ConfigFromSettings(settings.Settings{SearchProviders: []string{ProviderCustom}}, "")
	out := WithSearchAPIOverride(cfg, "https://api.example/s", "")
	if len(out.Order) != 1 {
		t.Fatalf("Order = %v, want custom listed once", out.Order)
	}
}

// TestSearchConfigVocabularyMatchesSettings pins the F6 inversion: the id
// strings settings validates on write and the ids this package's providers
// report must be the same set, because both now derive from the settings
// vocabulary.
func TestSearchConfigVocabularyMatchesSettings(t *testing.T) {
	want := settings.KnownSearchProviderIDs()
	got := KnownProviderIDs()
	if len(got) != len(want) {
		t.Fatalf("KnownProviderIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KnownProviderIDs[%d] = %q, want %q (vocabulary drift)", i, got[i], want[i])
		}
	}
}

// TestSearchConfigRoundTripsThroughSettings: what the settings page writes is
// what the search chain uses — the point of the whole exercise.
func TestSearchConfigRoundTripsThroughSettings(t *testing.T) {
	st := openSettingsStore(t)
	cfg := settings.Default()
	cfg.SearchProviders = []string{"tavily"}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got := ConfigFromSettings(st.Load(), "")
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
