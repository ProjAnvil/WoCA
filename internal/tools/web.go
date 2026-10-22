package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/security"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/websearch"
)

const maxWebChars = 20000

var tagRe = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)

// validateWebURL rejects URLs that fail the default SSRF policy before any
// network request is made.
func validateWebURL(rawurl string) error {
	if err := security.ValidateURL(rawurl, security.DefaultSSRFPolicy()); err != nil {
		return fmt.Errorf("blocked URL: %w", err)
	}
	return nil
}

// Test seam: the default SSRF policy hard-blocks loopback/private hosts, so
// hermetic tests cannot reach the fetch path at all. Tests may swap this var
// (restoring it afterwards); production behavior is identical to the default.
var webURLValidate = validateWebURL

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
	defer func() { _ = resp.Body.Close() }()
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
	t := MustTool(coretools.FromFunc("WebFetch", webFetchDescription,
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
		}))
	return t
}

const webSearchDescription = `Search the web and return a short list of titles, URLs, and snippets.`

// noResultsText is what the model sees for a search that succeeded and found
// nothing. It is deliberately unlike every error message: telling the model
// "no results" when a backend actually refused us makes it conclude the
// information does not exist and invent it instead.
const noResultsText = "No results found for this query."

type webSearchArgs struct {
	Query string `json:"query"`
}

// webSearchRouter is the process-wide router behind WebSearch. It starts as
// the built-in default chain and is replaced by SetWebSearchConfig whenever the
// settings change, so a settings edit takes effect on the next turn.
var (
	webSearchMu     sync.RWMutex
	webSearchRouter = websearch.NewRouter(websearch.Config{}.RouterOptions())
)

func webSearchTool() coretools.Tool { return newWebSearchTool(nil) }

// WebSearchRouter exposes the process-wide router so callers can read its
// per-provider stats (for SLA reporting) without rebuilding the chain.
func WebSearchRouter() *websearch.Router {
	webSearchMu.RLock()
	defer webSearchMu.RUnlock()
	return webSearchRouter
}

// SetWebSearchConfig rebuilds the search chain from cfg.
//
// Called at agent-build time and again on every tool refresh, which is what
// makes the settings page live: without it the chain would be frozen at
// process start. The cooldown table is intentionally not carried over — a
// settings change means the operator re-picked the backends, so stale
// penalties would only keep a just-fixed backend out of the chain.
func SetWebSearchConfig(cfg websearch.Config) {
	webSearchMu.Lock()
	defer webSearchMu.Unlock()
	webSearchRouter = websearch.NewRouter(cfg.RouterOptions())
}

// newWebSearchTool builds WebSearch over a router. This is the test seam that
// the provider-registry design exists for: the tool shell never changes, only
// what sits behind it.
//
// A nil router means "resolve the process-wide router on every call". That is
// how a rebuilt chain reaches an already-registered tool: the tool set is built
// once per agent, but the router behind it is swapped in place.
func newWebSearchTool(r *websearch.Router) coretools.Tool {
	t := MustTool(coretools.FromFunc("WebSearch", webSearchDescription,
		func(ctx context.Context, a webSearchArgs) (coretools.Result, error) {
			router := r
			if router == nil {
				router = WebSearchRouter()
			}
			results, err := router.Search(ctx, a.Query, 0)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("search %q: %w", a.Query, err)
			}
			out := websearch.Format(results)
			if out == "" {
				out = noResultsText
			}
			return coretools.Result{Content: out}, nil
		}))
	return t
}

// WebSearchToolWithProviders builds WebSearch over an explicit provider chain.
//
// Passing a single provider makes it an exclusive override, which is what
// --search-api means. A configured endpoint is an explicit choice; silently
// falling back from it to a public backend would send an operator's queries to
// a third party they did not select.
func WebSearchToolWithProviders(providers ...websearch.Provider) coretools.Tool {
	return newWebSearchTool(websearch.NewRouter(websearch.RouterOptions{Providers: providers}))
}
