package websearch

import (
	"slices"

	"github.com/projanvil/woca/internal/settings"
)

// ConfigFromSettings builds the web-search chain configuration from a
// settings snapshot.
//
// It lives here rather than on settings.Settings because the mapping is
// websearch knowledge and the dependency must point this way: a base
// storage package does not import capability packages (arch-cleanup F6 —
// this constructor used to be Settings.SearchConfig in internal/settings).
//
// customKey is passed in rather than read here because it is a credential: it
// lives in the secrets table (settings.SecretSearchCustomKey) and only the
// settings Store can read it. Keeping it out of settings.Settings means it
// never reaches a JSON payload, a log line, or the config plane.
func ConfigFromSettings(s settings.Settings, customKey string) Config {
	return Config{
		Order:          s.SearchProviders,
		SearXNG:        s.SearchSearXNGEndpoint,
		Custom:         s.SearchCustomEndpoint,
		CustomKey:      customKey,
		AllowedDomains: s.SearchAllowedDomains,
		DeniedDomains:  s.SearchDeniedDomains,
	}
}

// WithSearchAPIOverride applies a command-line --search-api to a config.
//
// The flag predates the settings surface and stays supported: it is explicit,
// per-invocation intent typed on the command line, which is a different thing
// from a value hidden in the environment. It overrides the configured custom
// endpoint for this run, and appends the custom backend to an explicit order
// so the flag cannot be silently ignored.
func WithSearchAPIOverride(cfg Config, endpoint, key string) Config {
	if endpoint == "" {
		return cfg
	}
	cfg.Custom = endpoint
	cfg.CustomKey = key
	if len(cfg.Order) > 0 && !containsProvider(cfg.Order, ProviderCustom) {
		cfg.Order = append(cfg.Order, ProviderCustom)
	}
	return cfg
}

func containsProvider(order []string, id string) bool {
	return slices.Contains(order, id)
}
