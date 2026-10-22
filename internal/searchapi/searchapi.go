// Package searchapi provides a client for generic JSON web-search APIs.
package searchapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Result is one search result.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// HTTPError is a non-200 response from the search API.
//
// It exposes StatusCode because callers need to tell throttling (429/503) apart
// from a genuine failure: only the former is worth failing over to another
// backend.
type HTTPError struct {
	StatusCode int
	Query      string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("search %q: HTTP %d", e.Query, e.StatusCode)
}

// Client queries a generic JSON web-search API.
type Client struct {
	endpoint   string
	apiKey     string
	maxResults int
	httpClient *http.Client
}

// Config configures a Client. Endpoint is the full search URL; "{q}" (if
// present) is replaced with the URL-encoded query, otherwise "?q=" is appended.
type Config struct {
	Endpoint   string
	APIKey     string // sent as Authorization: Bearer <key> when set
	MaxResults int    // default 8
	// HTTPClient is optional.
	HTTPClient *http.Client
}

const defaultMaxResults = 8

// New returns a Client configured with cfg. MaxResults defaults to 8 and
// HTTPClient defaults to http.DefaultClient.
func New(cfg Config) *Client {
	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		endpoint:   cfg.Endpoint,
		apiKey:     cfg.APIKey,
		maxResults: maxResults,
		httpClient: httpClient,
	}
}

// Search performs a query and parses the response.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.buildURL(query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "woca/0.1")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Query: query}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var payload response
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	results := payload.results()
	if len(results) > c.maxResults {
		results = results[:c.maxResults]
	}
	return results, nil
}

func (c *Client) buildURL(query string) string {
	encoded := url.QueryEscape(query)
	if strings.Contains(c.endpoint, "{q}") {
		return strings.ReplaceAll(c.endpoint, "{q}", encoded)
	}
	return c.endpoint + "?q=" + encoded
}

// response mirrors the expected JSON shape, accepting either "results" or
// "items" as the result list.
type response struct {
	Results []rawResult `json:"results"`
	Items   []rawResult `json:"items"`
}

func (r *response) results() []Result {
	raw := r.Results
	if len(raw) == 0 {
		raw = r.Items
	}
	out := make([]Result, 0, len(raw))
	for _, item := range raw {
		snippet := item.Snippet
		if snippet == "" {
			snippet = item.Description
		}
		out = append(out, Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: snippet,
		})
	}
	return out
}

// rawResult is a single result, accepting "snippet" or "description" for the
// snippet text.
type rawResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	Description string `json:"description"`
}

// Format renders results as "N. title\n   url\n   snippet" lines.
func Format(results []Result) string {
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return b.String()
}
