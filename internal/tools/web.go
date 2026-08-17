package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/security"
	coretools "github.com/projanvil/langchain-golang/core/tools"
)

const maxWebChars = 20000

var (
	tagRe     = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
	ddgLinkRe = regexp.MustCompile(`(?s)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnipRe = regexp.MustCompile(`(?s)<a[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	ddgFormRe = regexp.MustCompile(`<[^>]+>`)
)

// validateWebURL rejects URLs that fail the default SSRF policy before any
// network request is made.
func validateWebURL(rawurl string) error {
	if err := security.ValidateURL(rawurl, security.DefaultSSRFPolicy()); err != nil {
		return fmt.Errorf("blocked URL: %w", err)
	}
	return nil
}

// Test seams: the default SSRF policy hard-blocks loopback/private hosts and
// the search endpoint is a fixed remote URL, so hermetic tests cannot reach
// the fetch paths at all. Tests may swap these two vars (restoring them
// afterwards); production behavior is identical to the defaults below.
var (
	webURLValidate    = validateWebURL
	webSearchEndpoint = "https://html.duckduckgo.com/html/"
)

func fetchHTTP(ctx context.Context, rawurl string) (string, error) {
	if err := webURLValidate(rawurl); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; woca/0.1)")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func stripHTML(html string) string {
	out := tagRe.ReplaceAllString(html, " ")
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > maxWebChars {
		out = out[:maxWebChars] + "..."
	}
	return out
}

const webFetchDescription = `Fetch a URL and return its text content (HTML tags stripped, truncated).`

type webFetchArgs struct {
	URL string `json:"url"`
}

func webFetchTool() coretools.Tool {
	t, err := coretools.FromFunc("WebFetch", webFetchDescription,
		func(ctx context.Context, a webFetchArgs) (coretools.Result, error) {
			html, err := fetchHTTP(ctx, a.URL)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("fetch %s: %w", a.URL, err)
			}
			text := stripHTML(html)
			if text == "" {
				text = "(empty page)"
			}
			return coretools.Result{Content: text}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

const webSearchDescription = `Search the web and return a short list of titles, URLs, and snippets.`

type webSearchArgs struct {
	Query string `json:"query"`
}

func webSearchTool() coretools.Tool {
	t, err := coretools.FromFunc("WebSearch", webSearchDescription,
		func(ctx context.Context, a webSearchArgs) (coretools.Result, error) {
			q := url.Values{}
			q.Set("q", a.Query)
			html, err := fetchHTTP(ctx, webSearchEndpoint+"?"+q.Encode())
			if err != nil {
				return coretools.Result{}, fmt.Errorf("search %q: %w", a.Query, err)
			}
			out := parseDuckDuckGo(html)
			if out == "" {
				out = "No results found."
			}
			return coretools.Result{Content: out}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

func parseDuckDuckGo(html string) string {
	links := ddgLinkRe.FindAllStringSubmatch(html, -1)
	snips := ddgSnipRe.FindAllStringSubmatch(html, -1)
	n := len(links)
	if n > 8 {
		n = 8
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		href := cleanDuckDuckGoURL(links[i][1])
		title := ddgFormRe.ReplaceAllString(links[i][2], "")
		title = strings.TrimSpace(title)
		snippet := ""
		if i < len(snips) {
			snippet = strings.TrimSpace(ddgFormRe.ReplaceAllString(snips[i][1], ""))
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, href)
		if snippet != "" {
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
	}
	return b.String()
}

func cleanDuckDuckGoURL(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		if decoded, err := url.QueryUnescape(target); err == nil {
			return decoded
		}
		return target
	}
	return href
}

// SearchBackend runs a web search and returns formatted results.
type SearchBackend func(ctx context.Context, query string) (string, error)

// WebSearchToolWithBackend builds a WebSearch tool backed by a custom search
// backend (used to override the default DuckDuckGo backend with a real API).
func WebSearchToolWithBackend(backend SearchBackend) coretools.Tool {
	t, err := coretools.FromFunc("WebSearch", webSearchDescription,
		func(ctx context.Context, a webSearchArgs) (coretools.Result, error) {
			out, err := backend(ctx, a.Query)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("search %q: %w", a.Query, err)
			}
			if out == "" {
				out = "No results found."
			}
			return coretools.Result{Content: out}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}
