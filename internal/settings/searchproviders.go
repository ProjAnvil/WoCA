package settings

// Web-search provider id vocabulary.
//
// Ownership (arch-cleanup F6): the ids live here, in the storage layer,
// because Apply must reject unknown ids on write — a typo must not
// silently disable the chain — and a base storage package cannot import
// the websearch capability package to learn them. The websearch package
// consumes this vocabulary in reverse (its provider ids and
// KnownProviderIDs derive from these constants), so there is exactly one
// source for the strings and drift is a compile error, not a runtime
// surprise.
const (
	// SearchProviderDDGHTML / SearchProviderDDGLite are DuckDuckGo's two
	// endpoints; together they form the built-in default chain.
	SearchProviderDDGHTML = "ddg-html"
	SearchProviderDDGLite = "ddg-lite"
	// SearchProviderSearXNG requires a user-chosen instance endpoint.
	SearchProviderSearXNG = "searxng"
	// SearchProviderTavily is the keyless aggregate backend.
	SearchProviderTavily = "tavily"
	// SearchProviderGeneric is the user-supplied JSON search API (the
	// --search-api surface). websearch re-exports it as
	// websearch.ProviderCustom.
	SearchProviderGeneric = "generic"
)

// KnownSearchProviderIDs lists the ids accepted in SearchProviders.
func KnownSearchProviderIDs() []string {
	return []string{
		SearchProviderDDGHTML,
		SearchProviderDDGLite,
		SearchProviderSearXNG,
		SearchProviderTavily,
		SearchProviderGeneric,
	}
}
