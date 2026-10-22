package websearch

import (
	"fmt"
	"strings"
	"time"
)

// Config is a resolved web-search configuration.
//
// It is deliberately a plain value with no ambient state. It used to be
// populated from environment variables, which made the chain unconfigurable
// from the product UI and left two competing sources of truth. The settings
// layer (internal/settings) is now the single source and builds one of these.
type Config struct {
	// Order is the failover order, by provider id. Empty means the built-in
	// default: DuckDuckGo's two endpoints, then whichever opt-in backends are
	// configured.
	Order []string
	// SearXNG is the instance base URL. Required by the searxng provider,
	// which has no public default: choosing an instance on the user's behalf
	// would send their queries to a third party they did not pick.
	SearXNG string
	// Custom is a user-supplied JSON search API endpoint (the --search-api
	// surface). This is the extension point for user-defined routes: today a
	// single endpoint, later a named list.
	Custom string
	// CustomKey is the optional bearer key for Custom. It is a credential, so
	// callers pass it from the secrets store and it must never be logged.
	CustomKey string
	// AllowedDomains, when non-empty, drops results whose host is outside it.
	// DeniedDomains drops matching hosts and wins over AllowedDomains.
	AllowedDomains []string
	DeniedDomains  []string
	// MinInterval and Cooldown override the router defaults when positive.
	MinInterval time.Duration
	Cooldown    time.Duration
}

// Build constructs the provider chain. Backends whose prerequisites are not met
// (searxng without an endpoint, custom without an endpoint) are omitted; the
// router applies its own availability filter on top.
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
			out = append(out, NewTavily(TavilyOptions{}))
		case genericID:
			if c.Custom != "" {
				out = append(out, NewGeneric(GenericOptions{Endpoint: c.Custom, APIKey: c.CustomKey}))
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
// with a shorter chain than they asked for and no explanation. The settings
// layer rejects unknown ids on write; this is the read-path backstop.
func (c Config) Validate() error {
	known := map[string]bool{}
	for _, id := range KnownProviderIDs() {
		known[id] = true
	}
	var unknown []string
	for _, raw := range c.Order {
		if !known[strings.ToLower(strings.TrimSpace(raw))] {
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

// ProviderCustom is the id of the user-supplied JSON search API backend. It is
// exported so callers outside this package can put the extension point into an
// order without hardcoding the string.
const ProviderCustom = genericID

// order returns the effective chain: the explicit override, or the default —
// DuckDuckGo's two endpoints, then whichever configured backends apply.
func (c Config) order() []string {
	if len(c.Order) > 0 {
		return c.Order
	}
	ids := []string{ddgHTMLID, ddgLiteID}
	if c.SearXNG != "" {
		ids = append(ids, searxngID)
	}
	if c.Custom != "" {
		ids = append(ids, genericID)
	}
	return ids
}
