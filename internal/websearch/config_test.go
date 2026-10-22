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

func TestConfiguredProvidersJoinTheChain(t *testing.T) {
	got := ids(Config{SearXNG: "https://searx.example.com", Tavily: true}.Build())
	want := []string{ddgHTMLID, ddgLiteID, searxngID, tavilyID}
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

// TestOptInProvidersStayOutUnlessEnabled: Tavily is keyless but still a third
// party, so enabling it has to be a decision, not a default.
func TestOptInProvidersStayOutUnlessEnabled(t *testing.T) {
	got := ids(Config{Tavily: false, SearXNG: ""}.Build())
	for _, id := range got {
		if id == tavilyID || id == searxngID {
			t.Fatalf("chain %v contains an opt-in provider that was not enabled", got)
		}
	}
}

func TestValidateRejectsUnknownProvider(t *testing.T) {
	c := Config{Order: []string{"ddg-html", "brave"}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate(unknown) = nil, want error")
	} else if !strings.Contains(err.Error(), "brave") {
		t.Fatalf("Validate error = %v, want it to name the offending id", err)
	}
	if err := (Config{Order: []string{"searxng", tavilyID}}).Validate(); err != nil {
		t.Fatalf("Validate(known) = %v, want nil", err)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv(EnvProviders, "searxng, ddg-html")
	t.Setenv(EnvSearXNG, "https://searx.example.com")
	t.Setenv(EnvTavily, "on")
	t.Setenv(EnvAllowedDomains, "go.dev, example.com")
	t.Setenv(EnvDeniedDomains, "spam.example.net")

	c := LoadConfig()
	if len(c.Order) != 2 || c.Order[0] != "searxng" {
		t.Errorf("Order = %v", c.Order)
	}
	if c.SearXNG != "https://searx.example.com" {
		t.Errorf("SearXNG = %q", c.SearXNG)
	}
	if !c.Tavily {
		t.Error("Tavily should be enabled")
	}
	if len(c.AllowedDomains) != 2 || c.AllowedDomains[0] != "go.dev" {
		t.Errorf("AllowedDomains = %v", c.AllowedDomains)
	}
	if len(c.DeniedDomains) != 1 {
		t.Errorf("DeniedDomains = %v", c.DeniedDomains)
	}
	opts := c.RouterOptions()
	if len(opts.Providers) != 2 {
		t.Errorf("RouterOptions providers = %v", ids(opts.Providers))
	}
}

func TestLoadConfigEmptyEnv(t *testing.T) {
	t.Setenv(EnvProviders, "")
	t.Setenv(EnvSearXNG, "")
	t.Setenv(EnvTavily, "")
	t.Setenv(EnvAllowedDomains, "")
	t.Setenv(EnvDeniedDomains, "")

	c := LoadConfig()
	if c.Order != nil || c.SearXNG != "" || c.Tavily {
		t.Fatalf("empty env produced %+v", c)
	}
	if len(c.RouterOptions().Providers) != 2 {
		t.Fatalf("empty env chain = %v", ids(c.RouterOptions().Providers))
	}
}

func TestEnvBool(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		t.Setenv(EnvTavily, on)
		if !LoadConfig().Tavily {
			t.Errorf("envBool(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv(EnvTavily, off)
		if LoadConfig().Tavily {
			t.Errorf("envBool(%q) = true, want false", off)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList("a, b ,,c"); len(got) != 3 || got[1] != "b" {
		t.Fatalf("splitList = %v", got)
	}
	if got := splitList("  "); got != nil {
		t.Fatalf("splitList(blank) = %v, want nil", got)
	}
}
