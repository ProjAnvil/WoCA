package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// tavilyID identifies the provider in config, logs and stats.
const tavilyID = "tavily"

const tavilyEndpoint = "https://api.tavily.com/search"

// TavilyProvider calls Tavily's search API in keyless mode.
//
// Keyless mode is the reason this provider exists: it is the only
// no-credential backend that comes from a vendor with an actual availability
// commitment, so it is the natural second rung behind DuckDuckGo.
//
// There is deliberately no API-key option on this provider. An API key is a
// paid tier, and this branch is keyless-only by policy.
type TavilyProvider struct {
	endpoint string
	backend  httpBackend
}

// TavilyOptions configures the provider. Endpoint overrides the vendor URL,
// which tests need.
type TavilyOptions struct {
	Endpoint string
	Client   *http.Client
	UA       string
}

// NewTavily returns a keyless Tavily provider.
func NewTavily(opts TavilyOptions) *TavilyProvider {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = tavilyEndpoint
	}
	return &TavilyProvider{
		endpoint: endpoint,
		backend:  newHTTPBackend(opts.Client, opts.UA),
	}
}

func (p *TavilyProvider) ID() string   { return tavilyID }
func (p *TavilyProvider) Name() string { return "Tavily (keyless)" }

func (p *TavilyProvider) Available() bool {
	if p.endpoint == "" {
		return false
	}
	return endpointValidator(p.endpoint) == nil
}

func (p *TavilyProvider) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if query == "" {
		return nil, errors.New("empty search query")
	}
	status, header, body, err := p.backend.postJSON(ctx, p.endpoint, map[string]string{
		"X-Tavily-Access-Mode": "keyless",
		"X-Client-Name":        "woca",
	}, tavilyRequest{
		Query:           query,
		SearchDepth:     "basic",
		ChunksPerSource: 3,
		MaxResults:      clampLimit(limit),
	})
	if err != nil {
		return nil, err
	}
	if rl := classifyStatus(tavilyID, status, parseRetryAfter(header.Get("Retry-After"))); rl != nil {
		return nil, rl
	}
	var payload tavilyResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse %s response: %w", tavilyID, err)
	}
	results := make([]Result, 0, len(payload.Results))
	for _, item := range payload.Results {
		results = append(results, Result{
			Title:     item.Title,
			URL:       item.URL,
			Snippet:   item.Content,
			Content:   item.Content,
			Published: parseFlexibleTime(item.PublishedDate),
			Score:     item.Score,
		})
	}
	return clampResults(results, limit), nil
}

// tavilyRequest is the keyless search body. The shape mirrors what the vendor
// documents; search_depth is pinned to "basic" because keyless access is not
// meant to absorb the cost of deep searches.
type tavilyRequest struct {
	Query           string `json:"query"`
	SearchDepth     string `json:"search_depth"`
	ChunksPerSource int    `json:"chunks_per_source"`
	MaxResults      int    `json:"max_results"`
}

type tavilyResponse struct {
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date"`
	} `json:"results"`
}
