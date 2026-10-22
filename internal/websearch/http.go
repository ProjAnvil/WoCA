package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/projanvil/langchain-golang/core/security"
)

const (
	// defaultTimeout bounds a single backend call. Search is on the agent's
	// critical path, so this is deliberately shorter than the 30s used by
	// WebFetch: a slow backend should fail over, not stall the turn.
	defaultTimeout = 15 * time.Second

	// defaultUA identifies woca honestly. Rotating user agents is NOT an
	// anti-bot measure — DuckDuckGo's blocking is IP-reputation based, so a
	// pool of agents buys nothing. A single well-formed UA is the whole point.
	defaultUA = "Mozilla/5.0 (compatible; woca/0.3)"

	// maxResponseBytes caps a search response. Result pages are far smaller;
	// the cap exists so a misbehaving endpoint cannot exhaust memory.
	maxResponseBytes = 2 << 20
)

// endpointValidator gates provider endpoints that come from configuration.
//
// Search endpoints are normally trusted constants, but a configured endpoint
// (SearXNG, a corporate JSON API) is user-supplied, and pointing it at a
// metadata service would turn a search into an SSRF primitive. The same policy
// WebFetch uses is therefore applied here.
//
// Tests replace this to point providers at loopback servers.
var endpointValidator = func(rawurl string) error {
	return security.ValidateURL(rawurl, security.DefaultSSRFPolicy())
}

// httpBackend carries the transport settings shared by HTTP-based providers.
type httpBackend struct {
	client *http.Client
	ua     string
}

func newHTTPBackend(client *http.Client, ua string) httpBackend {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	if ua == "" {
		ua = defaultUA
	}
	return httpBackend{client: client, ua: ua}
}

// get issues a GET and returns the raw response material.
//
// It deliberately does not interpret the status: throttling, empty results and
// success are all HTTP 200 in practice, so callers must decide using
// classifyStatus and DetectAnomaly together.
func (b httpBackend) get(ctx context.Context, rawurl string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", b.ua)
	req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("read %s: %w", rawurl, err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// postJSON issues a POST with a JSON body, for backends whose query is too
// structured to live in a URL (Tavily).
func (b httpBackend) postJSON(ctx context.Context, rawurl string, headers map[string]string, payload any) (int, http.Header, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawurl, bytes.NewReader(data))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", b.ua)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("read %s: %w", rawurl, err)
	}
	return resp.StatusCode, resp.Header, body, nil
}
