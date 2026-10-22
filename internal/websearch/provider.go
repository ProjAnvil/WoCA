// Package websearch provides a pluggable web-search backend registry for the
// WebSearch tool.
//
// The tool shell is fixed; backends are interchangeable. That split is what
// lets a free, keyless chain (DuckDuckGo, SearXNG, Tavily keyless) coexist with
// a configured JSON API without any of them knowing about each other.
//
// Two rules govern error handling, and they exist because conflating them makes
// models hallucinate:
//
//   - Being rate-limited/blocked is an ERROR (ErrRateLimited), never an empty
//     result. A model told "No results found" concludes the information does
//     not exist and invents it.
//   - A genuine empty result set is a SUCCESS carrying zero results.
package websearch

import (
	"context"
	"errors"
	"time"
)

// Result is one search result.
//
// Title/URL/Snippet are required; every backend must fill them. The remaining
// fields are optional and are only populated by backends that natively return
// them — scraping backends leave them zero.
type Result struct {
	Title   string
	URL     string
	Snippet string

	// Content is an optional body excerpt. Populating it is an optimization
	// (one fewer WebFetch round-trip), never a dependency: WebFetch must keep
	// working when the search backend is down.
	Content string
	// Published is the publication time when the backend reports one.
	Published *time.Time
	// Score is backend-reported relevance in [0,1]; 0 means "not provided".
	Score float64
}

// Provider is one search backend.
type Provider interface {
	// ID is the stable, lowercase identifier used in config, logs and stats
	// (for example "ddg-html", "searxng", "tavily").
	ID() string
	// Name is the human-readable label shown to users.
	Name() string
	// Available reports whether this provider is configured well enough to be
	// used. Providers carry their config at construction time, so this takes
	// no arguments.
	Available() bool
	// Search runs a query. limit is advisory: a provider may return fewer
	// results, and must not treat it as a minimum.
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

var (
	// ErrNoBackend means no provider is configured at all. It is NOT worth
	// failing over — there is nothing to fail over to.
	ErrNoBackend = errors.New("no search backend configured; set --search-api, " +
		"or enable a provider (searxng / tavily keyless)")

	// ErrRateLimited means the backend is throttling this machine. The wording
	// is deliberately directive: an unqualified "search failed" makes models
	// retry with rephrased queries, which deepens the throttle.
	ErrRateLimited = errors.New("search backend is rate-limiting this machine; " +
		"do not retry or rephrase; wait a few minutes or fetch known URLs directly")
)

// RateLimitError is the structured form of ErrRateLimited. It carries the
// provider that was throttled and, when the backend said so, how long to stay
// away from it.
type RateLimitError struct {
	Provider string
	// RetryAfter is the backend-requested cooldown. Zero means "use the
	// router default".
	RetryAfter time.Duration
	// Reason is the diagnostic detail (status code, marker, ...) kept out of
	// the message the model sees.
	Reason string
}

// Error implements error. The text is fixed regardless of Reason so that
// identical failures collapse into one message the model can act on.
func (e *RateLimitError) Error() string {
	if e.Provider == "" {
		return ErrRateLimited.Error()
	}
	return e.Provider + ": " + ErrRateLimited.Error()
}

// Is makes errors.Is(err, ErrRateLimited) true for any *RateLimitError.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Unwrap exposes ErrRateLimited to errors.Unwrap chains.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// Failover reports whether err means "this backend is unusable right now, try
// another one". Only rate-limit style failures qualify.
//
// Everything else — network errors, 401/403, non-503 5xx — is returned to the
// caller as-is. Hopping providers on arbitrary errors would hide real
// misconfiguration behind apparent flakiness.
func Failover(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}
