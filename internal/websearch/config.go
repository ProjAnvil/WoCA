package websearch

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Environment variables that configure the search chain.
//
// These are a first surface, not the final one: woca has a settings layer that
// does not expose search at all yet. Env vars are what CI and containers can
// set today; the settings keys should land on top of this Config, not replace
// it.
const (
	// EnvProviders sets the chain order explicitly, comma-separated, for
	// example "searxng,ddg-html". Unset means the built-in default order.
	EnvProviders = "WOCA_SEARCH_PROVIDERS"
	// EnvSearXNG is the instance base URL, for example
	// "https://searx.example.com". Required by the searxng provider, which has
	// no public default: choosing an instance on the user's behalf would send
	// their queries to a third party they did not pick.
	EnvSearXNG = "WOCA_SEARCH_SEARXNG"
	// EnvTavily enables the keyless Tavily provider. Off by default because it
	// is still a third party.
	EnvTavily = "WOCA_SEARCH_TAVILY"
	// EnvAllowedDomains / EnvDeniedDomains are comma-separated host filters.
	EnvAllowedDomains = "WOCA_SEARCH_ALLOWED_DOMAINS"
	EnvDeniedDomains  = "WOCA_SEARCH_DENIED_DOMAINS"
)

// Config is a resolved search configuration.
type Config struct {
	// Order overrides the provider chain, by provider id. Empty means default.
	Order          []string
	SearXNG        string
	Tavily         bool
	AllowedDomains []string
	DeniedDomains  []string
	// MinInterval and Cooldown override the router defaults when positive.
	MinInterval time.Duration
	Cooldown    time.Duration
}

// LoadConfig reads the search configuration from the environment.
func LoadConfig() Config {
	c := Config{
		SearXNG:        strings.TrimSpace(os.Getenv(EnvSearXNG)),
		Tavily:         envBool(EnvTavily),
		AllowedDomains: splitList(os.Getenv(EnvAllowedDomains)),
		DeniedDomains:  splitList(os.Getenv(EnvDeniedDomains)),
	}
	if v := os.Getenv(EnvProviders); v != "" {
		c.Order = splitList(v)
	}
	return c
}

// Build constructs the provider chain. Providers whose prerequisites are not
// met (SearXNG without an endpoint, Tavily not enabled) are omitted; the router
// applies its own availability filter on top.
func (c Config) Build() []Provider {
	out := make([]Provider, 0, len(c.Order)+4)
	for _, raw := range c.order() {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case ddgHTMLID:
			out = append(out, NewDDGHTML(DDGOptions{}))
		case ddgLiteID:
			out = append(out, NewDDGLite(DDGOptions{}))
		case searxngID:
			if c.SearXNG != "" {
				out = append(out, NewSearXNG(SearXNGOptions{Endpoint: c.SearXNG}))
			}
		case tavilyID:
			if c.Tavily {
				out = append(out, NewTavily(TavilyOptions{}))
			}
		}
	}
	return out
}

// RouterOptions turns the config into router settings.
func (c Config) RouterOptions() RouterOptions {
	return RouterOptions{
		Providers:      c.Build(),
		MinInterval:    c.MinInterval,
		Cooldown:       c.Cooldown,
		AllowedDomains: c.AllowedDomains,
		DeniedDomains:  c.DeniedDomains,
	}
}

// Validate reports provider ids in Order that this build does not know.
//
// An unknown id is silently dropped by Build, which would leave an operator
// with a shorter chain than they asked for and no explanation. Call this
// wherever configuration can be surfaced to a human.
func (c Config) Validate() error {
	var unknown []string
	for _, raw := range c.Order {
		id := strings.ToLower(strings.TrimSpace(raw))
		switch id {
		case ddgHTMLID, ddgLiteID, searxngID, tavilyID:
		default:
			unknown = append(unknown, raw)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown search provider(s) %q; known: %s",
		strings.Join(unknown, ", "), strings.Join(KnownProviderIDs(), ", "))
}

// KnownProviderIDs lists the ids accepted in Order.
func KnownProviderIDs() []string {
	return []string{ddgHTMLID, ddgLiteID, searxngID, tavilyID, genericID}
}

// order returns the effective chain: the explicit override, or the default —
// DuckDuckGo's two endpoints, then whichever opt-in backends are configured.
func (c Config) order() []string {
	if len(c.Order) > 0 {
		return c.Order
	}
	ids := []string{ddgHTMLID, ddgLiteID}
	if c.SearXNG != "" {
		ids = append(ids, searxngID)
	}
	if c.Tavily {
		ids = append(ids, tavilyID)
	}
	return ids
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
