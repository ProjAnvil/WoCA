package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// searxngID identifies the provider in config, logs and stats.
const searxngID = "searxng"

// SearXNGProvider queries a SearXNG instance's JSON API.
//
// SearXNG is the only keyless option with a clean terms-of-use story: it is
// self-hostable, so an enterprise deployment can point woca at an internal
// instance and keep queries off third-party infrastructure entirely. That is
// why it takes an endpoint rather than a default URL.
type SearXNGProvider struct {
	endpoint string
	backend  httpBackend
}

// SearXNGOptions configures the provider. Endpoint is required — there is no
// public default, because picking one on the user's behalf would silently send
// their queries to a third party they did not choose.
type SearXNGOptions struct {
	Endpoint string
	Client   *http.Client
	UA       string
}

// NewSearXNG returns a SearXNG provider for the given instance.
func NewSearXNG(opts SearXNGOptions) *SearXNGProvider {
	return &SearXNGProvider{
		endpoint: opts.Endpoint,
		backend:  newHTTPBackend(opts.Client, opts.UA),
	}
}

func (p *SearXNGProvider) ID() string   { return searxngID }
func (p *SearXNGProvider) Name() string { return "SearXNG" }

// Available reports whether an endpoint was configured and passes the SSRF
// policy. Without an endpoint the provider drops out of the chain.
func (p *SearXNGProvider) Available() bool {
	if p.endpoint == "" {
		return false
	}
	return endpointValidator(p.endpoint) == nil
}

func (p *SearXNGProvider) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("empty search query")
	}
	status, header, body, err := p.backend.get(ctx, p.buildURL(query))
	if err != nil {
		return nil, err
	}
	// Most public instances disable the JSON format and answer 403. A bare
	// "HTTP 403" leaves the operator guessing, so name the cause.
	if status == http.StatusForbidden {
		return nil, errors.New("SearXNG returned HTTP 403; the instance likely disables " +
			"format=json — choose an instance that enables it, or self-host")
	}
	if rl := classifyStatus(searxngID, status, parseRetryAfter(header.Get("Retry-After"))); rl != nil {
		return nil, rl
	}
	var payload searxngResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse %s response: %w", searxngID, err)
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

// buildURL supports both a bare instance URL and a template containing "{q}",
// and adds format=json only when it is not already present.
func (p *SearXNGProvider) buildURL(query string) string {
	encoded := url.QueryEscape(query)
	if strings.Contains(p.endpoint, "{q}") {
		u := strings.ReplaceAll(p.endpoint, "{q}", encoded)
		if !strings.Contains(u, "format=json") {
			u = appendQuery(u, "format=json")
		}
		return u
	}
	u := appendQuery(p.endpoint, "q="+encoded)
	if !strings.Contains(u, "format=json") {
		u = appendQuery(u, "format=json")
	}
	return u
}

func appendQuery(rawurl, kv string) string {
	sep := "?"
	if strings.Contains(rawurl, "?") {
		sep = "&"
	}
	return rawurl + sep + kv
}

// searxngResponse mirrors the subset of the SearXNG JSON API that is used.
type searxngResponse struct {
	Results []struct {
		URL           string  `json:"url"`
		Title         string  `json:"title"`
		Content       string  `json:"content"`
		PublishedDate string  `json:"publishedDate"`
		Score         float64 `json:"score"`
	} `json:"results"`
}

// publishedTimeLayouts are the date shapes SearXNG instances emit. Dates are
// optional metadata: an unparseable one is dropped rather than failing the
// search.
var publishedTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	time.RFC1123,
	time.RFC1123Z,
	"2006-01-02",
	"02 Jan 2006",
}

func parseFlexibleTime(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	for _, layout := range publishedTimeLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}
