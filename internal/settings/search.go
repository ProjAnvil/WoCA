package settings

import (
	"slices"

	"github.com/projanvil/woca/internal/websearch"
)

// SearchConfig builds the web-search chain configuration from a settings
// snapshot.
//
// customKey is passed in rather than read here because it is a credential: it
// lives in the secrets table (SecretSearchCustomKey) and only the Store can
// read it. Keeping it out of Settings means it never reaches a JSON payload,
// a log line, or the config plane.
func (s Settings) SearchConfig(customKey string) websearch.Config {
	return websearch.Config{
		Order:          s.SearchProviders,
		SearXNG:        s.SearchSearXNGEndpoint,
		Custom:         s.SearchCustomEndpoint,
		CustomKey:      customKey,
		AllowedDomains: s.SearchAllowedDomains,
		DeniedDomains:  s.SearchDeniedDomains,
	}
}

// SearchConfigWithOverride applies a command-line --search-api to a config.
//
// The flag predates the settings surface and stays supported: it is explicit,
// per-invocation intent typed on the command line, which is a different thing
// from a value hidden in the environment. It overrides the configured custom
// endpoint for this run, and appends the custom backend to an explicit order
// so the flag cannot be silently ignored.
func SearchConfigWithOverride(cfg websearch.Config, endpoint, key string) websearch.Config {
	if endpoint == "" {
		return cfg
	}
	cfg.Custom = endpoint
	cfg.CustomKey = key
	if len(cfg.Order) > 0 && !containsProvider(cfg.Order, websearch.ProviderCustom) {
		cfg.Order = append(cfg.Order, websearch.ProviderCustom)
	}
	return cfg
}

func containsProvider(order []string, id string) bool {
	return slices.Contains(order, id)
}
