package websearch

import (
	"strings"
	"testing"
)

func ids(providers []Provider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.ID())
	}
	return out
}

func TestDefaultChainIsDuckDuckGoOnly(t *testing.T) {
	// Zero-config must work, and must not silently add a third party the user
	// did not opt into.
	got := ids(Config{}.Build())
	want := []string{ddgHTMLID, ddgLiteID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default chain = %v, want %v", got, want)
	}
}

// TestConfiguredBackendsJoinTheChain: enabling is expressed by listing the
// provider id, so ordering and enable/disable are one control.
func TestConfiguredBackendsJoinTheChain(t *testing.T) {
	c := Config{
		Order:   []string{ddgHTMLID, ddgLiteID, searxngID, tavilyID, genericID},
		SearXNG: "https://searx.example.com",
		Custom:  "https://search.example/api",
	}
	got := ids(c.Build())
	want := []string{ddgHTMLID, ddgLiteID, searxngID, tavilyID, genericID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

func TestExplicitOrderIsRespected(t *testing.T) {
	c := Config{
		Order:   []string{"searxng", "ddg-html"},
		SearXNG: "https://searx.example.com",
	}
	got := ids(c.Build())
	want := []string{searxngID, ddgHTMLID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// TestOptInProvidersStayOutUnlessListed: Tavily is keyless but still a third
// party, so turning it on has to be a decision, not a default.
func TestOptInProvidersStayOutUnlessListed(t *testing.T) {
	got := ids(Config{SearXNG: "", Custom: ""}.Build())
	for _, id := range got {
		if id == tavilyID || id == searxngID || id == genericID {
			t.Fatalf("chain %v contains a provider that was not enabled", got)
		}
	}
}

// TestListedButUnconfiguredIsDropped: listing searxng without an endpoint (or
// custom without an endpoint) must drop it rather than produce a provider that
// cannot work.
func TestListedButUnconfiguredIsDropped(t *testing.T) {
	c := Config{Order: []string{searxngID, genericID, ddgHTMLID}}
	got := ids(c.Build())
	want := []string{ddgHTMLID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

func TestValidateRejectsUnknownProvider(t *testing.T) {
	c := Config{Order: []string{"ddg-html", "brave"}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate(unknown) = nil, want error")
	} else if !strings.Contains(err.Error(), "brave") {
		t.Fatalf("Validate error = %v, want it to name the offending id", err)
	}
	if err := (Config{Order: []string{"searxng", tavilyID, genericID}}).Validate(); err != nil {
		t.Fatalf("Validate(known) = %v, want nil", err)
	}
}

func TestKnownProviderIDsIncludeCustom(t *testing.T) {
	// The settings layer validates against this list, so the custom extension
	// point has to be in it or a user's own endpoint would be rejected.
	var found bool
	for _, id := range KnownProviderIDs() {
		if id == genericID {
			found = true
		}
	}
	if !found {
		t.Fatalf("KnownProviderIDs = %v, want it to include %q", KnownProviderIDs(), genericID)
	}
}

func TestRouterOptionsCarriesFiltersThrough(t *testing.T) {
	opts := Config{
		AllowedDomains: []string{"go.dev"},
		DeniedDomains:  []string{"spam.example"},
	}.RouterOptions()
	if len(opts.AllowedDomains) != 1 || opts.AllowedDomains[0] != "go.dev" {
		t.Errorf("AllowedDomains = %v", opts.AllowedDomains)
	}
	if len(opts.DeniedDomains) != 1 || opts.DeniedDomains[0] != "spam.example" {
		t.Errorf("DeniedDomains = %v", opts.DeniedDomains)
	}
}

// TestNoEnvironmentVariables: configuration used to come from WOCA_SEARCH_*
// environment variables, which left two competing sources of truth. The
// settings layer is now the only source, so Config must have no ambient state.
func TestNoEnvironmentVariables(t *testing.T) {
	t.Setenv("WOCA_SEARCH_PROVIDERS", "searxng")
	t.Setenv("WOCA_SEARCH_SEARXNG", "https://from-env.example")
	t.Setenv("WOCA_SEARCH_TAVILY", "1")
	t.Setenv("WOCA_SEARCH_ALLOWED_DOMAINS", "env.example")
	t.Setenv("WOCA_SEARCH_DENIED_DOMAINS", "env.example")

	got := ids(Config{}.Build())
	want := []string{ddgHTMLID, ddgLiteID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("env leaked into the chain: got %v, want %v", got, want)
	}
}
