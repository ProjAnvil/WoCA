package server

// The web-search configuration API backing the settings panel's web-search
// section. GET /api/websearch lists the backend catalog with enable flags and
// the failover order, PUT /api/websearch persists the selection and rebinds
// the chain live, and POST /api/websearch/test runs one real query so a user
// can confirm the current configuration actually returns results.
//
// The test endpoint is the point of the whole section: a backend that is
// throttled or blocked looks identical to a search that found nothing, so
// without it a user cannot tell "my settings are wrong" from "the internet is
// broken".

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/websearch"
)

// defaultTestQuery is used when the caller does not supply one. It is common
// enough that a working backend will return something.
const defaultTestQuery = "golang programming language"

// providerCatalog describes a backend for the UI. It lives here rather than in
// internal/websearch because it is presentation: labels, and whether the
// backend needs an endpoint before it can be enabled.
type providerCatalog struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	NeedsEndpoint bool   `json:"needs_endpoint"`
}

// catalog lists every backend the UI can offer, in the order they are shown
// before the user reorders them.
func catalog() []providerCatalog {
	return []providerCatalog{
		{
			ID:          "ddg-html",
			Name:        "DuckDuckGo",
			Description: "No account needed. Blocked from some datacenter and corporate networks.",
		},
		{
			ID:          "ddg-lite",
			Name:        "DuckDuckGo (lightweight)",
			Description: "A different DuckDuckGo endpoint, useful as a fallback when the main one is blocked.",
		},
		{
			ID:            "searxng",
			Name:          "SearXNG",
			Description:   "Self-hosted or public SearXNG instance. Needs an endpoint.",
			NeedsEndpoint: true,
		},
		{
			ID:          "tavily",
			Name:        "Tavily (keyless)",
			Description: "No API key needed, but queries still go to a third party.",
		},
		{
			ID:            "custom",
			Name:          "Custom endpoint",
			Description:   "Any JSON search API you host yourself. Needs an endpoint.",
			NeedsEndpoint: true,
		},
	}
}

type websearchProviderView struct {
	providerCatalog
	Enabled bool `json:"enabled"`
	Order   int  `json:"order"`
}

type websearchResponse struct {
	Providers []websearchProviderView `json:"providers"`
	// Enabled is the failover order, a subset of the catalog ids.
	Enabled         []string `json:"enabled"`
	SearXNGEndpoint string   `json:"searxng_endpoint"`
	CustomEndpoint  string   `json:"custom_endpoint"`
	// CustomKeySet reports whether a key is stored; the key itself is never
	// returned.
	CustomKeySet   bool     `json:"custom_key_set"`
	AllowedDomains []string `json:"allowed_domains"`
	DeniedDomains  []string `json:"denied_domains"`
	// ManagedFields lists keys locked by the organization, so the UI can
	// render them read-only.
	ManagedFields []string `json:"managed_fields"`
}

func (s *Server) handleGetWebSearch(w http.ResponseWriter, _ *http.Request) {
	cfg := settings.Settings{}
	managed := []string{}
	if s.settings != nil {
		loaded := s.settings.Load()
		cfg = loaded
		managed = searchManagedFields(s.settings.ManagedFields())
	}

	// Effective order, not the raw setting: an unset setting still means the
	// two DuckDuckGo endpoints, and the UI has to be able to show and reorder
	// them.
	enabled := cfg.SearchConfig("").EffectiveOrder()
	rank := map[string]int{}
	for i, id := range enabled {
		rank[id] = i + 1
	}
	views := make([]websearchProviderView, 0, len(catalog()))
	for _, p := range catalog() {
		order, on := rank[p.ID]
		views = append(views, websearchProviderView{providerCatalog: p, Enabled: on, Order: order})
	}

	var keySet bool
	if s.settings != nil {
		keySet = s.settings.HasSecret(settings.SecretSearchCustomKey)
	}
	if enabled == nil {
		enabled = []string{}
	}
	writeJSON(w, http.StatusOK, websearchResponse{
		Providers:       views,
		Enabled:         enabled,
		SearXNGEndpoint: cfg.SearchSearXNGEndpoint,
		CustomEndpoint:  cfg.SearchCustomEndpoint,
		CustomKeySet:    keySet,
		AllowedDomains:  nonNilStrings(cfg.SearchAllowedDomains),
		DeniedDomains:   nonNilStrings(cfg.SearchDeniedDomains),
		ManagedFields:   managed,
	})
}

// websearchUpdate is the PUT payload. Every field is a full replacement of the
// search block, matching how PUT /api/plugins replaces the enabled list.
type websearchUpdate struct {
	Providers       []string `json:"providers"`
	SearXNGEndpoint string   `json:"searxng_endpoint"`
	CustomEndpoint  string   `json:"custom_endpoint"`
	// CustomKey is write-only: empty means "leave the stored key alone",
	// matching the secret convention used by PUT /api/settings. There is no
	// way to clear it here on purpose — clearing credentials over a partial
	// update is how people lock themselves out.
	CustomKey      string   `json:"custom_key"`
	AllowedDomains []string `json:"allowed_domains"`
	DeniedDomains  []string `json:"denied_domains"`
}

func (s *Server) handlePutWebSearch(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	var req websearchUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := s.settings.Load()
	// Apply validates: unknown provider ids and duplicates are rejected, and
	// endpoints must parse as http(s) URLs.
	for _, step := range []struct {
		key, value string
	}{
		{settings.KeySearchProviders, settings.FormatNameList(req.Providers)},
		{settings.KeySearchSearXNGEndpoint, req.SearXNGEndpoint},
		{settings.KeySearchCustomEndpoint, req.CustomEndpoint},
		{settings.KeySearchAllowedDomains, settings.FormatNameList(req.AllowedDomains)},
		{settings.KeySearchDeniedDomains, settings.FormatNameList(req.DeniedDomains)},
	} {
		if err := settings.Apply(&cfg, step.key, step.value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.CustomKey != "" {
		if err := s.settings.SetSecret(settings.SecretSearchCustomKey, req.CustomKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Rebind the chain. RefreshTools re-reads the settings and re-applies the
	// --search-api override, so it is preferred; without an agent (tests,
	// embedders) the chain is rebuilt directly.
	if s.agent != nil {
		s.agent.RefreshToolsNow(r.Context())
	} else {
		tools.SetWebSearchConfig(cfg.SearchConfig(s.settings.GetSecret(settings.SecretSearchCustomKey)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type websearchTestResponse struct {
	OK        bool   `json:"ok"`
	Provider  string `json:"provider,omitempty"`
	Results   int    `json:"results"`
	LatencyMS int64  `json:"latency_ms"`
	Sample    string `json:"sample,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleTestWebSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = defaultTestQuery
	}

	cfg := settings.Settings{}
	if s.settings != nil {
		cfg = s.settings.Load()
	}
	var key string
	if s.settings != nil {
		key = s.settings.GetSecret(settings.SecretSearchCustomKey)
	}

	// A throwaway router, not the shared one: the probe must not disturb the
	// live cooldown table, and it needs its own event sink to report which
	// backend served the query.
	var served string
	opts := cfg.SearchConfig(key).RouterOptions()
	opts.OnEvent = func(e websearch.Event) {
		if e.Kind == websearch.EventSuccess {
			served = e.Provider
		}
	}
	router := websearch.NewRouter(opts)

	start := time.Now()
	results, err := router.Search(r.Context(), query, 5)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		writeJSON(w, http.StatusOK, websearchTestResponse{
			OK: false, LatencyMS: latency, Provider: served, Error: err.Error(),
		})
		return
	}
	var sample string
	if len(results) > 0 {
		sample = results[0].Title + " — " + results[0].URL
	}
	writeJSON(w, http.StatusOK, websearchTestResponse{
		OK: true, Provider: served, Results: len(results),
		LatencyMS: latency, Sample: sample,
	})
}

// searchManagedFields narrows the managed key list to the search ones.
func searchManagedFields(managed []string) []string {
	searchKeys := map[string]bool{
		settings.KeySearchProviders:       true,
		settings.KeySearchSearXNGEndpoint: true,
		settings.KeySearchCustomEndpoint:  true,
		settings.KeySearchCustomKey:       true,
		settings.KeySearchAllowedDomains:  true,
		settings.KeySearchDeniedDomains:   true,
	}
	out := []string{}
	for _, k := range managed {
		if searchKeys[k] {
			out = append(out, k)
		}
	}
	return out
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
