package settings

import (
	"strings"
	"testing"
)

func TestSearchSettingsRoundTrip(t *testing.T) {
	st := openTest(t)
	cfg := Default()
	cfg.SearchProviders = []string{"searxng", "ddg-html"}
	cfg.SearchSearXNGEndpoint = "https://searx.internal.example.com"
	cfg.SearchCustomEndpoint = "https://search.internal/api"
	cfg.SearchAllowedDomains = []string{"go.dev"}
	cfg.SearchDeniedDomains = []string{"spam.example"}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got := st.Load()
	if len(got.SearchProviders) != 2 || got.SearchProviders[0] != "searxng" {
		t.Fatalf("SearchProviders = %v, want [searxng ddg-html] (order is the failover order)", got.SearchProviders)
	}
	if got.SearchSearXNGEndpoint != "https://searx.internal.example.com" {
		t.Errorf("SearchSearXNGEndpoint = %q", got.SearchSearXNGEndpoint)
	}
	if got.SearchCustomEndpoint != "https://search.internal/api" {
		t.Errorf("SearchCustomEndpoint = %q", got.SearchCustomEndpoint)
	}
	if len(got.SearchAllowedDomains) != 1 || got.SearchAllowedDomains[0] != "go.dev" {
		t.Errorf("SearchAllowedDomains = %v", got.SearchAllowedDomains)
	}
	if len(got.SearchDeniedDomains) != 1 || got.SearchDeniedDomains[0] != "spam.example" {
		t.Errorf("SearchDeniedDomains = %v", got.SearchDeniedDomains)
	}
}

// TestSearchSettingsClearingPersists guards the empty-means-skip trap: the
// Save loop skips empty values, so a removed endpoint needs an explicit
// delete or it comes back on the next load.
func TestSearchSettingsClearingPersists(t *testing.T) {
	st := openTest(t)

	cfg := Default()
	cfg.SearchProviders = []string{"searxng"}
	cfg.SearchSearXNGEndpoint = "https://searx.internal.example.com"
	cfg.SearchCustomEndpoint = "https://search.internal/api"
	cfg.SearchAllowedDomains = []string{"go.dev"}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	cfg.SearchProviders = nil
	cfg.SearchSearXNGEndpoint = ""
	cfg.SearchCustomEndpoint = ""
	cfg.SearchAllowedDomains = nil
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got := st.Load()
	if len(got.SearchProviders) != 0 {
		t.Errorf("SearchProviders = %v, want empty", got.SearchProviders)
	}
	if got.SearchSearXNGEndpoint != "" {
		t.Errorf("SearchSearXNGEndpoint = %q, want empty (not persisted)", got.SearchSearXNGEndpoint)
	}
	if got.SearchCustomEndpoint != "" {
		t.Errorf("SearchCustomEndpoint = %q, want empty (not persisted)", got.SearchCustomEndpoint)
	}
	if len(got.SearchAllowedDomains) != 0 {
		t.Errorf("SearchAllowedDomains = %v, want empty", got.SearchAllowedDomains)
	}
}

func TestApplySearchProviders(t *testing.T) {
	var cfg Settings
	if err := Apply(&cfg, KeySearchProviders, `["searxng","ddg-html"]`); err != nil {
		t.Fatal(err)
	}
	if len(cfg.SearchProviders) != 2 || cfg.SearchProviders[1] != "ddg-html" {
		t.Fatalf("SearchProviders = %v", cfg.SearchProviders)
	}
}

// TestApplySearchProvidersRejectsUnknown: a typo must not silently disable
// the whole chain — same rule the plugin API applies.
func TestApplySearchProvidersRejectsUnknown(t *testing.T) {
	var cfg Settings
	err := Apply(&cfg, KeySearchProviders, `["brave"]`)
	if err == nil {
		t.Fatal("unknown provider accepted")
	}
	if !strings.Contains(err.Error(), "unknown web-search provider") {
		t.Errorf("error = %q, want it to name the bad id", err)
	}
}

func TestApplySearchProvidersRejectsDuplicates(t *testing.T) {
	var cfg Settings
	if err := Apply(&cfg, KeySearchProviders, `["ddg-html","ddg-html"]`); err == nil {
		t.Fatal("duplicate provider accepted")
	}
}

func TestApplySearchEndpointValidation(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
	}{
		{"https://searx.example.com", true},
		{"http://searx.internal:8080", true},
		{"", true}, // clearing
		{"ftp://searx.example.com", false},
		{"searx.example.com", false},
		{"https://", false},
	}
	for _, c := range cases {
		var cfg Settings
		err := Apply(&cfg, KeySearchSearXNGEndpoint, c.value)
		if c.ok && err != nil {
			t.Errorf("Apply(%q) = %v, want nil", c.value, err)
		}
		if !c.ok && err == nil {
			t.Errorf("Apply(%q) = nil, want error", c.value)
		}
	}
}

func TestApplySearchCustomKeyIsSecret(t *testing.T) {
	var cfg Settings
	if err := Apply(&cfg, KeySearchCustomKey, "sk-test"); err == nil {
		t.Fatal("the custom search key must be rejected by Apply (it is a secret)")
	}
	if _, ok := SecretFor(KeySearchCustomKey); !ok {
		t.Error("SecretFor(search-custom-key) should resolve")
	}
}

func TestSearchCustomKeyRoundTrip(t *testing.T) {
	st := openTest(t)
	if err := st.SetSecret(SecretSearchCustomKey, "sk-live"); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSecret(SecretSearchCustomKey); got != "sk-live" {
		t.Fatalf("GetSecret = %q, want sk-live", got)
	}
	if !st.HasSecret(SecretSearchCustomKey) {
		t.Error("HasSecret should be true")
	}
}

// TestSearchSettingsSurviveNormalize: Normalize must not clobber the search
// fields, or a settings edit elsewhere would silently reset the chain.
func TestSearchSettingsSurviveNormalize(t *testing.T) {
	cfg := Default()
	cfg.SearchProviders = []string{"tavily"}
	cfg.SearchSearXNGEndpoint = "https://searx.example.com"
	out, err := cfg.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SearchProviders) != 1 || out.SearchProviders[0] != "tavily" {
		t.Errorf("SearchProviders after Normalize = %v", out.SearchProviders)
	}
	if out.SearchSearXNGEndpoint != "https://searx.example.com" {
		t.Errorf("SearchSearXNGEndpoint after Normalize = %q", out.SearchSearXNGEndpoint)
	}
}

// TestKeyValueSearchKeys exercises the CLI/TUI read path.
func TestKeyValueSearchKeys(t *testing.T) {
	cfg := Settings{
		SearchProviders:       []string{"ddg-html"},
		SearchSearXNGEndpoint: "https://searx.example",
		SearchCustomEndpoint:  "https://api.example",
		SearchAllowedDomains:  []string{"go.dev"},
		SearchDeniedDomains:   []string{"spam.example"},
	}
	for _, k := range []string{
		KeySearchProviders, KeySearchSearXNGEndpoint,
		KeySearchCustomEndpoint, KeySearchAllowedDomains, KeySearchDeniedDomains,
	} {
		if v := KeyValue(cfg, k); v == "" {
			t.Errorf("KeyValue(%s) is empty", k)
		}
	}
}

// TestSearchKeysHaveHelp satisfies the KeyNames/KeyHelp consistency rule the
// package enforces for every listed key.
func TestSearchKeysHaveHelp(t *testing.T) {
	for _, k := range []string{
		KeySearchProviders, KeySearchSearXNGEndpoint,
		KeySearchCustomEndpoint, KeySearchCustomKey,
		KeySearchAllowedDomains, KeySearchDeniedDomains,
	} {
		if h := KeyHelp(k); h == "" || h == "unknown key" {
			t.Errorf("KeyHelp(%s) = %q", k, h)
		}
	}
}

func TestSearchKeysAreListed(t *testing.T) {
	listed := map[string]bool{}
	for _, k := range KeyNames() {
		listed[k] = true
	}
	for _, k := range []string{
		KeySearchProviders, KeySearchSearXNGEndpoint,
		KeySearchCustomEndpoint, KeySearchCustomKey,
		KeySearchAllowedDomains, KeySearchDeniedDomains,
	} {
		if !listed[k] {
			t.Errorf("%s missing from KeyNames()", k)
		}
	}
}
