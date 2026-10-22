package websearch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Provider identifiers, stable across releases because they appear in config,
// logs and stats.
const (
	ddgHTMLID = "ddg-html"
	ddgLiteID = "ddg-lite"
)

const (
	ddgHTMLEndpoint = "https://html.duckduckgo.com/html/"
	ddgLiteEndpoint = "https://lite.duckduckgo.com/lite/"
)

const (
	// defaultMaxResults is what a backend is asked for when the caller does not
	// care. Eight keeps the tool output small enough to be cheap in context.
	defaultMaxResults = 8
	// maxResultsHardCap bounds what a caller may ask for. Backends bill or
	// throttle by result count, so an unbounded limit is a footgun.
	maxResultsHardCap = 20
)

// DDGProvider scrapes one of DuckDuckGo's no-key HTML endpoints.
//
// Both endpoints return HTML rather than JSON and have no published contract,
// so the selectors are the fragile part of this provider; they are isolated
// here and covered by tests that use saved markup.
type DDGProvider struct {
	id, name string
	endpoint string
	// containerSel identifies one result block and is how a title anchor gets
	// paired with its snippet. When it matches nothing the provider falls back
	// to pairing by document order.
	containerSel string
	linkSel      string
	snipSel      string
	backend      httpBackend
}

// DDGOptions configures a DDG provider. Endpoint overrides the built-in URL,
// which tests and operators pointing at a mirror need.
type DDGOptions struct {
	Endpoint string
	Client   *http.Client
	UA       string
}

// NewDDGHTML returns the provider for the full HTML endpoint.
func NewDDGHTML(opts DDGOptions) *DDGProvider {
	return newDDG(ddgHTMLID, "DuckDuckGo (html)", ddgHTMLEndpoint,
		".result__body", "a.result__a", "a.result__snippet", opts)
}

// NewDDGLite returns the provider for the lightweight endpoint. It is a
// different page shape, so it acts as a genuine second chance rather than a
// retry of the same request.
func NewDDGLite(opts DDGOptions) *DDGProvider {
	return newDDG(ddgLiteID, "DuckDuckGo (lite)", ddgLiteEndpoint,
		"", "a.result-link", ".result-snippet", opts)
}

func newDDG(id, name, endpoint, container, link, snip string, opts DDGOptions) *DDGProvider {
	if opts.Endpoint != "" {
		endpoint = opts.Endpoint
	}
	return &DDGProvider{
		id:           id,
		name:         name,
		endpoint:     endpoint,
		containerSel: container,
		linkSel:      link,
		snipSel:      snip,
		backend:      newHTTPBackend(opts.Client, opts.UA),
	}
}

func (p *DDGProvider) ID() string   { return p.id }
func (p *DDGProvider) Name() string { return p.name }

// Available reports whether the endpoint is set and passes the SSRF policy.
func (p *DDGProvider) Available() bool {
	if p.endpoint == "" {
		return false
	}
	return endpointValidator(p.endpoint) == nil
}

func (p *DDGProvider) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("empty search query")
	}
	target, err := p.buildURL(query)
	if err != nil {
		return nil, err
	}
	status, header, body, err := p.backend.get(ctx, target)
	if err != nil {
		return nil, err
	}
	if rl := classifyStatus(p.id, status, parseRetryAfter(header.Get("Retry-After"))); rl != nil {
		return nil, rl
	}
	results := p.parse(body)
	// Zero results plus challenge markers means we were served an anti-bot
	// page, not an empty answer. Reporting this as "no results" is the bug
	// this package exists to fix.
	if len(results) == 0 && DetectAnomaly(string(body)) {
		return nil, &RateLimitError{Provider: p.id, Reason: "anti-bot interstitial"}
	}
	return clampResults(results, limit), nil
}

func (p *DDGProvider) buildURL(query string) (string, error) {
	if _, err := url.Parse(p.endpoint); err != nil {
		return "", err
	}
	sep := "?"
	if strings.Contains(p.endpoint, "?") {
		sep = "&"
	}
	return p.endpoint + sep + url.Values{"q": {query}}.Encode(), nil
}

// parse extracts results from a response body.
//
// Container pairing is tried first because it is correct: a result whose
// snippet is missing still keeps its own title. When the container selector
// matches nothing — a markup change, or the lite endpoint's table layout — it
// falls back to pairing the Nth title with the Nth snippet, which is what the
// previous regex implementation did.
func (p *DDGProvider) parse(body []byte) []Result {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	if p.containerSel != "" {
		var out []Result
		doc.Find(p.containerSel).Each(func(_ int, s *goquery.Selection) {
			link := s.Find(p.linkSel).First()
			if link.Length() == 0 {
				return
			}
			href, _ := link.Attr("href")
			if href == "" {
				return
			}
			out = append(out, Result{
				Title:   collapseSpace(link.Text()),
				URL:     unwrapDDGURL(href),
				Snippet: collapseSpace(s.Find(p.snipSel).First().Text()),
			})
		})
		if len(out) > 0 {
			return out
		}
	}
	return p.parseByIndex(doc)
}

func (p *DDGProvider) parseByIndex(doc *goquery.Document) []Result {
	titles := doc.Find(p.linkSel)
	snippets := doc.Find(p.snipSel)
	out := make([]Result, 0, titles.Length())
	titles.Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if href == "" {
			return
		}
		r := Result{Title: collapseSpace(s.Text()), URL: unwrapDDGURL(href)}
		if i < snippets.Length() {
			r.Snippet = collapseSpace(snippets.Eq(i).Text())
		}
		out = append(out, r)
	})
	return out
}

// unwrapDDGURL resolves DuckDuckGo's redirect wrapper to the real target.
// Results are useless to a model if every URL points back at DuckDuckGo.
func unwrapDDGURL(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	target := u.Query().Get("uddg")
	if target == "" {
		return href
	}
	// The wrapper is double-encoded. If the second unescape fails (a malformed
	// escape such as "%zz") the once-unescaped value is still the closest thing
	// to the real target, so it is returned as-is.
	if decoded, err := url.QueryUnescape(target); err == nil {
		return decoded
	}
	return target
}

// collapseSpace flattens the whitespace that survives tag stripping.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clampLimit applies the default and hard cap to a requested result count.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultMaxResults
	}
	if limit > maxResultsHardCap {
		return maxResultsHardCap
	}
	return limit
}

// clampResults truncates a result set to the clamped limit.
func clampResults(in []Result, limit int) []Result {
	if limit = clampLimit(limit); len(in) > limit {
		return in[:limit]
	}
	return in
}
