package configsvc

// The config plane's web-search probe: run one real query with the chain the
// console is about to ship, and report which backend answered.
//
// Without it an operator is guessing. A throttled backend looks exactly like
// a search that found nothing, so "the settings are wrong" and "this network
// cannot reach DuckDuckGo" are indistinguishable from the settings page — and
// the second one is the common case on managed networks.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/websearch"
)

// defaultTestQuery is used when the operator does not supply one.
const defaultTestQuery = "golang programming language"

type websearchTestResponse struct {
	OK        bool   `json:"ok"`
	Provider  string `json:"provider,omitempty"`
	Results   int    `json:"results"`
	LatencyMS int64  `json:"latency_ms"`
	Sample    string `json:"sample,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Service) handleTestWebSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = defaultTestQuery
	}

	cfg := s.settings.Load()
	// A throwaway router: the console's probe must not disturb the config
	// service's own cooldown table (there is none, but keeping the probe
	// isolated means it also works before any settings are published).
	var served string
	opts := cfg.SearchConfig(s.settings.GetSecret(settings.SecretSearchCustomKey)).RouterOptions()
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
