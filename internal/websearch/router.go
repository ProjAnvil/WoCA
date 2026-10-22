package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// defaultMinInterval throttles outbound searches. Free backends throttle by
	// IP reputation, and a burst of automated queries from one host is exactly
	// the signal they look for.
	defaultMinInterval = time.Second

	// defaultCooldown is how long a provider stays out of the chain after it
	// reports throttling, when it did not say how long itself.
	defaultCooldown = time.Minute

	// maxCooldown caps a backend-supplied Retry-After. A hostile or broken
	// server could otherwise park a provider for a day and quietly force every
	// query onto whichever backend it likes.
	maxCooldown = 10 * time.Minute
)

// RouterOptions configures a Router.
type RouterOptions struct {
	Providers []Provider
	// MinInterval throttles the whole router: consecutive searches are spaced
	// by at least this much regardless of which provider serves them.
	MinInterval time.Duration
	// Cooldown is applied to a provider that reports throttling without a
	// Retry-After of its own.
	Cooldown time.Duration
	// AllowedDomains, when non-empty, drops results whose host is outside it.
	// DeniedDomains drops matching hosts and wins over AllowedDomains.
	AllowedDomains []string
	DeniedDomains  []string
	// Now overrides the clock; tests advance it to exercise cooldowns.
	Now func() time.Time
	// OnEvent receives a telemetry event per attempt. Optional.
	OnEvent func(Event)
}

// Router dispatches a query across the configured providers.
//
// The failover rule is deliberately narrow: only throttling (429/503/202, or an
// anti-bot interstitial) moves to the next provider. Everything else returns
// immediately, because hopping providers on arbitrary failures turns
// misconfiguration into intermittent-looking flakiness that nobody can debug.
type Router struct {
	mu        sync.Mutex
	providers []Provider
	cooldowns map[string]time.Time
	stats     map[string]*ProviderStats
	limiter   *rate.Limiter
	cooldown  time.Duration
	allowed   []string
	denied    []string
	now       func() time.Time
	onEvent   func(Event)
}

// NewRouter builds a router over opts.Providers, keeping only those that are
// available.
func NewRouter(opts RouterOptions) *Router {
	minInterval := opts.MinInterval
	if minInterval <= 0 {
		minInterval = defaultMinInterval
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	providers := make([]Provider, 0, len(opts.Providers))
	stats := make(map[string]*ProviderStats, len(opts.Providers))
	for _, p := range opts.Providers {
		if p == nil || !p.Available() {
			continue
		}
		providers = append(providers, p)
		stats[p.ID()] = &ProviderStats{}
	}
	return &Router{
		providers: providers,
		cooldowns: make(map[string]time.Time),
		stats:     stats,
		limiter:   rate.NewLimiter(rate.Every(minInterval), 1),
		cooldown:  cooldown,
		allowed:   opts.AllowedDomains,
		denied:    opts.DeniedDomains,
		now:       now,
		onEvent:   opts.OnEvent,
	}
}

// Providers returns the providers currently in the chain, in priority order.
func (r *Router) Providers() []Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

func (r *Router) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if err := r.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	providers := r.Providers()
	if len(providers) == 0 {
		r.emit(Event{Kind: EventNoBackend})
		return nil, ErrNoBackend
	}

	var (
		lastRateLimit error
		attempted     int
		skipped       int
	)
	for _, p := range providers {
		if _, cooling := r.cooldownUntil(p.ID()); cooling {
			skipped++
			r.emit(Event{Kind: EventSkipped, Provider: p.ID(), Detail: "cooling down"})
			continue
		}
		attempted++
		start := r.now()
		results, err := p.Search(ctx, query, limit)
		elapsed := r.now().Sub(start)

		switch {
		case err == nil:
			r.recordSuccess(p.ID(), elapsed, len(results))
			filtered := r.filterDomains(results)
			if len(results) > 0 && len(filtered) == 0 {
				r.emit(Event{Kind: EventFilteredAll, Provider: p.ID(), Results: len(results)})
			}
			return filtered, nil

		case Failover(err):
			// Throttled: back this provider off and try the next one.
			var rl *RateLimitError
			errors.As(err, &rl)
			d := r.cooldown
			if rl != nil && rl.RetryAfter > 0 {
				d = min(rl.RetryAfter, maxCooldown)
			}
			r.cool(p.ID(), d)
			r.recordRateLimited(p.ID(), elapsed)
			lastRateLimit = err

		default:
			r.recordFailure(p.ID(), elapsed, err)
			return nil, fmt.Errorf("%s: %w", p.ID(), err)
		}
	}

	if attempted == 0 && skipped > 0 {
		// Everything is cooling down. That is still throttling, so it must not
		// be reported as "no backend configured".
		return nil, &RateLimitError{Reason: "all providers are cooling down"}
	}
	if lastRateLimit != nil {
		return nil, lastRateLimit
	}
	return nil, ErrNoBackend
}

func (r *Router) cool(id string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldowns[id] = r.now().Add(d)
}

func (r *Router) cooldownUntil(id string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.cooldowns[id]
	if !ok {
		return time.Time{}, false
	}
	// Expired cooldowns are dropped so Stats() does not report stale state.
	if !r.now().Before(until) {
		delete(r.cooldowns, id)
		return time.Time{}, false
	}
	return until, true
}

// filterDomains applies the domain policy to results BEFORE they reach the
// model. Filtering after loading would mean external content entered the
// context first and was then judged unusable.
func (r *Router) filterDomains(in []Result) []Result {
	if len(r.allowed) == 0 && len(r.denied) == 0 {
		return in
	}
	out := make([]Result, 0, len(in))
	for _, res := range in {
		host := hostOf(res.URL)
		if host != "" && domainMatches(r.denied, host) {
			continue
		}
		if len(r.allowed) > 0 && !domainMatches(r.allowed, host) {
			continue
		}
		out = append(out, res)
	}
	return out
}

// domainMatches reports whether host equals domain or is a subdomain of it.
func domainMatches(domains []string, host string) bool {
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func hostOf(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// eventHook is the process-wide telemetry sink for routers that were not given
// their own OnEvent. It exists because routers are built in more than one place
// (the default chain, and the --search-api override); without a package-level
// sink, events from the override would be invisible.
var eventHook func(Event)

// SetEventHook installs the process-wide event sink. Pass nil to disable.
func SetEventHook(fn func(Event)) { eventHook = fn }

func (r *Router) emit(e Event) {
	sink := r.onEvent
	if sink == nil {
		sink = eventHook
	}
	if sink == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = r.now()
	}
	sink(e)
}

// EventKind classifies a router telemetry event.
type EventKind string

const (
	EventSuccess     EventKind = "success"
	EventRateLimited EventKind = "rate_limited"
	EventFailure     EventKind = "failure"
	EventSkipped     EventKind = "skipped"
	EventFilteredAll EventKind = "filtered_all"
	EventNoBackend   EventKind = "no_backend"
)

// Event is one router observation, for logging or metrics export.
type Event struct {
	Time     time.Time
	Provider string
	Kind     EventKind
	Detail   string
	Latency  time.Duration
	Results  int
	Err      error
}

// ProviderStats is the per-backend SLA record. Availability is derivable as
// Successes/Attempts, and rate-limit pressure as RateLimited/Attempts.
type ProviderStats struct {
	Attempts     int64
	Successes    int64
	RateLimited  int64
	Failures     int64
	Results      int64
	TotalLatency time.Duration
	LastError    string
	LastSuccess  time.Time
	LastFailure  time.Time
}

// Stats snapshots per-provider counters.
func (r *Router) Stats() map[string]ProviderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]ProviderStats, len(r.stats))
	for id, s := range r.stats {
		out[id] = *s
	}
	return out
}

// statLocked returns the counter set for id, creating it on first use. Callers
// must hold r.mu.
func (r *Router) statLocked(id string) *ProviderStats {
	s, ok := r.stats[id]
	if !ok {
		s = &ProviderStats{}
		r.stats[id] = s
	}
	return s
}

// record* helpers mutate counters under the lock and emit outside it, so an
// OnEvent callback that re-enters the router cannot deadlock.
func (r *Router) recordSuccess(id string, elapsed time.Duration, n int) {
	r.mu.Lock()
	s := r.statLocked(id)
	s.Attempts++
	s.Successes++
	s.Results += int64(n)
	s.TotalLatency += elapsed
	s.LastSuccess = r.now()
	r.mu.Unlock()
	r.emit(Event{Provider: id, Kind: EventSuccess, Latency: elapsed, Results: n})
}

func (r *Router) recordRateLimited(id string, elapsed time.Duration) {
	r.mu.Lock()
	s := r.statLocked(id)
	s.Attempts++
	s.RateLimited++
	s.TotalLatency += elapsed
	s.LastFailure = r.now()
	s.LastError = ErrRateLimited.Error()
	r.mu.Unlock()
	r.emit(Event{Provider: id, Kind: EventRateLimited, Latency: elapsed, Err: ErrRateLimited})
}

func (r *Router) recordFailure(id string, elapsed time.Duration, err error) {
	r.mu.Lock()
	s := r.statLocked(id)
	s.Attempts++
	s.Failures++
	s.TotalLatency += elapsed
	s.LastFailure = r.now()
	s.LastError = err.Error()
	r.mu.Unlock()
	r.emit(Event{Provider: id, Kind: EventFailure, Latency: elapsed, Err: err})
}
