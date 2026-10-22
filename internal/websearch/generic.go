package websearch

import (
	"context"
	"errors"
	"net/http"

	"github.com/projanvil/woca/internal/searchapi"
)

// genericID identifies the user-supplied JSON endpoint in stats and logs.
const genericID = "generic"

// GenericProvider adapts a user-supplied JSON search API (--search-api) to the
// Provider interface, so a configured endpoint gets the same formatting,
// cooldown bookkeeping and telemetry as the built-in free backends.
type GenericProvider struct {
	endpoint string
	client   *searchapi.Client
}

// GenericOptions configures the adapter.
type GenericOptions struct {
	Endpoint string
	APIKey   string
	Client   *http.Client
}

// NewGeneric returns a provider backed by a generic JSON search endpoint.
func NewGeneric(opts GenericOptions) *GenericProvider {
	return &GenericProvider{
		endpoint: opts.Endpoint,
		client: searchapi.New(searchapi.Config{
			Endpoint:   opts.Endpoint,
			APIKey:     opts.APIKey,
			HTTPClient: opts.Client,
		}),
	}
}

func (p *GenericProvider) ID() string   { return genericID }
func (p *GenericProvider) Name() string { return "Configured search API" }

func (p *GenericProvider) Available() bool {
	if p.endpoint == "" {
		return false
	}
	return endpointValidator(p.endpoint) == nil
}

func (p *GenericProvider) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = defaultMaxResults
	}
	raw, err := p.client.Search(ctx, query)
	if err != nil {
		// Surface throttling as a rate-limit so the router can cool this
		// provider down. Everything else stays a plain failure.
		var he *searchapi.HTTPError
		if errors.As(err, &he) && isRateLimitStatus(he.StatusCode) {
			return nil, &RateLimitError{Provider: genericID, Reason: err.Error()}
		}
		return nil, err
	}
	results := make([]Result, 0, len(raw))
	for _, r := range raw {
		results = append(results, Result{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return clampResults(results, limit), nil
}
