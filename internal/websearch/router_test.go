package websearch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeProvider answers a scripted way so router behaviour can be tested without
// a network. It lives in the test file because nothing in production uses it.
type fakeProvider struct {
	id      string
	avail   bool
	results []Result
	err     error
	calls   int
	lastQ   string
	lastLim int
}

func (f *fakeProvider) ID() string      { return f.id }
func (f *fakeProvider) Name() string    { return f.id }
func (f *fakeProvider) Available() bool { return f.avail }

func (f *fakeProvider) Search(_ context.Context, query string, limit int) ([]Result, error) {
	f.calls++
	f.lastQ = query
	f.lastLim = limit
	return f.results, f.err
}

func newFake(id string, results []Result, err error) *fakeProvider {
	return &fakeProvider{id: id, avail: true, results: results, err: err}
}

// fixedClock returns a settable clock and the function to advance it, so
// cooldown expiry is testable without sleeping.
func fixedClock() (func() time.Time, func(time.Duration)) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func TestRouterNoBackend(t *testing.T) {
	r := NewRouter(RouterOptions{})
	if _, err := r.Search(t.Context(), "go", 0); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("empty router = %v, want ErrNoBackend", err)
	}
}

// TestRouterDropsUnavailableProviders: a provider that is not configured must
// not be dialled and must not count against the chain.
func TestRouterDropsUnavailableProviders(t *testing.T) {
	unavailable := &fakeProvider{id: "off", avail: false}
	ok := newFake("on", []Result{{Title: "T"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{unavailable, ok}, MinInterval: time.Millisecond})

	got, err := r.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || unavailable.calls != 0 {
		t.Fatalf("unavailable provider was used (calls=%d), results=%d", unavailable.calls, len(got))
	}
	if len(r.Providers()) != 1 {
		t.Fatalf("chain has %d providers, want 1", len(r.Providers()))
	}
}

func TestRouterUsesFirstAvailable(t *testing.T) {
	first := newFake("first", []Result{{Title: "A"}}, nil)
	second := newFake("second", []Result{{Title: "B"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{first, second}, MinInterval: time.Millisecond})

	got, err := r.Search(t.Context(), "go", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "A" || second.calls != 0 {
		t.Fatalf("got %+v, second calls %d", got, second.calls)
	}
	if first.lastQ != "go" || first.lastLim != 7 {
		t.Errorf("query/limit not forwarded: %q / %d", first.lastQ, first.lastLim)
	}
}

// TestRouterFailsOverOnRateLimit is the core degradation case.
func TestRouterFailsOverOnRateLimit(t *testing.T) {
	first := newFake("first", nil, &RateLimitError{Provider: "first"})
	second := newFake("second", []Result{{Title: "B"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{first, second}, MinInterval: time.Millisecond})

	got, err := r.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatalf("failover failed: %v", err)
	}
	if len(got) != 1 || got[0].Title != "B" {
		t.Fatalf("results = %+v, want the second provider's", got)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("calls = %d / %d, want 1 / 1", first.calls, second.calls)
	}
}

// TestRouterDoesNotFailOverOnHardError: a 500 is the backend's fault, not a
// reason to silently hop. Hopping here would hide misconfiguration.
func TestRouterDoesNotFailOverOnHardError(t *testing.T) {
	first := newFake("first", nil, errors.New("HTTP 500"))
	second := newFake("second", []Result{{Title: "B"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{first, second}, MinInterval: time.Millisecond})

	if _, err := r.Search(t.Context(), "go", 0); err == nil {
		t.Fatal("hard error = nil")
	}
	if second.calls != 0 {
		t.Error("second provider must not be tried after a non-failover error")
	}
}

func TestRouterCooldownSkipsThrottledProvider(t *testing.T) {
	now, advance := fixedClock()
	first := newFake("first", []Result{{Title: "A"}}, nil)
	first.err = &RateLimitError{Provider: "first"}
	second := newFake("second", []Result{{Title: "B"}}, nil)
	r := NewRouter(RouterOptions{
		Providers:   []Provider{first, second},
		MinInterval: time.Millisecond,
		Cooldown:    30 * time.Second,
		Now:         now,
	})

	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	// Still inside the cooldown: the throttled provider must be skipped rather
	// than retried, which would deepen the throttle.
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if first.calls != 1 {
		t.Errorf("throttled provider called %d times, want 1 (skipped while cooling)", first.calls)
	}

	// Once the cooldown lapses it becomes eligible again.
	advance(31 * time.Second)
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if first.calls != 2 {
		t.Errorf("after cooldown expiry first.calls = %d, want 2", first.calls)
	}
}

// TestRouterAllCoolingIsRateLimited: with every provider cooling, reporting
// "no backend configured" would send the operator chasing the wrong problem.
func TestRouterAllCoolingIsRateLimited(t *testing.T) {
	now, _ := fixedClock()
	first := newFake("first", nil, &RateLimitError{Provider: "first"})
	second := newFake("second", nil, &RateLimitError{Provider: "second"})
	r := NewRouter(RouterOptions{
		Providers:   []Provider{first, second},
		MinInterval: time.Millisecond,
		Cooldown:    time.Hour,
		Now:         now,
	})

	if _, err := r.Search(t.Context(), "go", 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first pass = %v, want ErrRateLimited", err)
	}
	if _, err := r.Search(t.Context(), "go", 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("all-cooling = %v, want ErrRateLimited (not ErrNoBackend)", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("calls = %d / %d, want 1 / 1 (no retries while cooling)", first.calls, second.calls)
	}
}

// TestRouterCapsBackendCooldown: a hostile Retry-After must not be able to
// park a provider indefinitely and steer all traffic to one backend.
func TestRouterCapsBackendCooldown(t *testing.T) {
	now, advance := fixedClock()
	first := newFake("first", []Result{{Title: "A"}}, nil)
	first.err = &RateLimitError{Provider: "first", RetryAfter: 24 * time.Hour}
	second := newFake("second", []Result{{Title: "B"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{first, second}, MinInterval: time.Millisecond, Now: now})

	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	advance(maxCooldown + time.Second)
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if first.calls != 2 {
		t.Errorf("after cap expiry first.calls = %d, want 2 (Retry-After was capped)", first.calls)
	}
}

func TestRouterDomainFiltering(t *testing.T) {
	results := []Result{
		{Title: "keep", URL: "https://go.dev/doc"},
		{Title: "sub", URL: "https://pkg.go.dev/x"},
		{Title: "deny", URL: "https://spam.example.net/p"},
		{Title: "other", URL: "https://example.com/a"},
	}
	allow := NewRouter(RouterOptions{
		Providers:      []Provider{newFake("p", results, nil)},
		MinInterval:    time.Millisecond,
		AllowedDomains: []string{"go.dev"},
	})
	got, err := allow.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("allow-list kept %d results, want 2: %+v", len(got), got)
	}

	deny := NewRouter(RouterOptions{
		Providers:     []Provider{newFake("p", results, nil)},
		MinInterval:   time.Millisecond,
		DeniedDomains: []string{"spam.example.net"},
	})
	got, err = deny.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("deny-list kept %d results, want 3: %+v", len(got), got)
	}

	// Denied wins over allowed, so a policy conflict fails closed.
	both := NewRouter(RouterOptions{
		Providers:      []Provider{newFake("p", results, nil)},
		MinInterval:    time.Millisecond,
		AllowedDomains: []string{"go.dev", "spam.example.net"},
		DeniedDomains:  []string{"spam.example.net"},
	})
	got, err = both.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("conflicting policy kept %d results, want 2", len(got))
	}
}

func TestDomainMatchesSubdomains(t *testing.T) {
	cases := []struct {
		domains []string
		host    string
		want    bool
	}{
		{[]string{"go.dev"}, "go.dev", true},
		{[]string{"go.dev"}, "pkg.go.dev", true},
		{[]string{"go.dev"}, "go.dev.evil.com", false},
		{[]string{".go.dev"}, "pkg.go.dev", true},
		{[]string{"GO.DEV"}, "pkg.go.dev", true},
		{nil, "anything", false},
	}
	for _, c := range cases {
		if got := domainMatches(c.domains, c.host); got != c.want {
			t.Errorf("domainMatches(%v, %q) = %v, want %v", c.domains, c.host, got, c.want)
		}
	}
}

func TestHostOf(t *testing.T) {
	if got := hostOf("https://Example.COM:8443/a?b=1"); got != "example.com" {
		t.Errorf("hostOf = %q, want example.com", got)
	}
	if got := hostOf("not a url"); got != "" {
		t.Errorf("hostOf(invalid) = %q, want empty", got)
	}
}

// TestRouterZeroResultsIsSuccess: an empty result set must come back as a
// successful empty slice, never as an error.
func TestRouterZeroResultsIsSuccess(t *testing.T) {
	r := NewRouter(RouterOptions{
		Providers:   []Provider{newFake("p", nil, nil)},
		MinInterval: time.Millisecond,
	})
	got, err := r.Search(t.Context(), "zzzz", 0)
	if err != nil {
		t.Fatalf("zero results = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0", len(got))
	}
}

func TestRouterStats(t *testing.T) {
	throttled := newFake("throttled", nil, &RateLimitError{Provider: "throttled"})
	good := newFake("good", []Result{{Title: "A"}, {Title: "B"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{throttled, good}, MinInterval: time.Millisecond})

	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	stats := r.Stats()
	if stats["throttled"].RateLimited != 1 || stats["throttled"].Attempts != 1 {
		t.Errorf("throttled stats = %+v", stats["throttled"])
	}
	if stats["good"].Successes != 1 || stats["good"].Results != 2 {
		t.Errorf("good stats = %+v", stats["good"])
	}
	if stats["good"].LastSuccess.IsZero() {
		t.Error("LastSuccess should be recorded")
	}
}

func TestRouterStatsRecordsFailures(t *testing.T) {
	broken := newFake("broken", nil, errors.New("HTTP 500"))
	r := NewRouter(RouterOptions{Providers: []Provider{broken}, MinInterval: time.Millisecond})
	if _, err := r.Search(t.Context(), "go", 0); err == nil {
		t.Fatal("expected error")
	}
	s := r.Stats()["broken"]
	if s.Failures != 1 || s.Successes != 0 || s.LastError == "" {
		t.Fatalf("failure stats = %+v", s)
	}
}

func TestRouterEmitsEvents(t *testing.T) {
	var kinds []EventKind
	throttled := newFake("throttled", nil, &RateLimitError{Provider: "throttled"})
	good := newFake("good", []Result{{Title: "A"}}, nil)
	r := NewRouter(RouterOptions{
		Providers:   []Provider{throttled, good},
		MinInterval: time.Millisecond,
		OnEvent:     func(e Event) { kinds = append(kinds, e.Kind) },
	})
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	seen := map[EventKind]bool{}
	for _, k := range kinds {
		seen[k] = true
	}
	if !seen[EventRateLimited] || !seen[EventSuccess] {
		t.Fatalf("events = %v, want rate_limited and success", kinds)
	}
}

// TestSetEventHookReachesLaterRouters: routers are built in more than one
// place (the default chain and the --search-api override), so the package-level
// sink has to reach routers created after it is installed.
func TestSetEventHookReachesLaterRouters(t *testing.T) {
	orig := eventHook
	t.Cleanup(func() { eventHook = orig })

	var kinds []EventKind
	SetEventHook(func(e Event) { kinds = append(kinds, e.Kind) })
	r := NewRouter(RouterOptions{
		Providers:   []Provider{newFake("p", []Result{{Title: "A"}}, nil)},
		MinInterval: time.Millisecond,
	})
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if len(kinds) == 0 {
		t.Fatal("global hook received no events")
	}
}

// TestRouterOnEventOverridesGlobalHook: a per-router sink must win, so a caller
// can scope telemetry without disturbing the process-wide one.
func TestRouterOnEventOverridesGlobalHook(t *testing.T) {
	orig := eventHook
	t.Cleanup(func() { eventHook = orig })
	SetEventHook(func(Event) { panic("global hook must not be used when OnEvent is set") })

	var got int
	r := NewRouter(RouterOptions{
		Providers:   []Provider{newFake("p", []Result{{Title: "A"}}, nil)},
		MinInterval: time.Millisecond,
		OnEvent:     func(Event) { got++ },
	})
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatal("per-router hook received no events")
	}
}

func TestRouterFilteredAllEvent(t *testing.T) {
	var got []Event
	r := NewRouter(RouterOptions{
		Providers:      []Provider{newFake("p", []Result{{Title: "A", URL: "https://x.com"}}, nil)},
		MinInterval:    time.Millisecond,
		AllowedDomains: []string{"go.dev"},
		OnEvent:        func(e Event) { got = append(got, e) },
	})
	if _, err := r.Search(t.Context(), "go", 0); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, e := range got {
		if e.Kind == EventFilteredAll {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected a filtered_all event, got %+v", got)
	}
}

// TestRouterThrottlesAcrossProviders: the limiter is per-router, not
// per-provider, so switching backends cannot be used to burst.
func TestRouterThrottlesAcrossProviders(t *testing.T) {
	r := NewRouter(RouterOptions{
		Providers:   []Provider{newFake("a", []Result{{Title: "A"}}, nil), newFake("b", []Result{{Title: "B"}}, nil)},
		MinInterval: 150 * time.Millisecond,
	})
	start := time.Now()
	for range 3 {
		if _, err := r.Search(t.Context(), "go", 0); err != nil {
			t.Fatal(err)
		}
	}
	// First call is free (bucket starts full); the next two each wait one
	// interval, so 3 calls must take at least 2 intervals.
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("3 searches took %v, want >= 300ms of throttling", elapsed)
	}
}

func TestRouterContextCancellation(t *testing.T) {
	r := NewRouter(RouterOptions{Providers: []Provider{newFake("p", nil, nil)}, MinInterval: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// The limiter is the first thing to observe cancellation.
	if _, err := r.Search(ctx, "go", 0); err == nil {
		t.Fatal("cancelled context = nil error")
	}
}

func TestRouterConcurrentSearch(t *testing.T) {
	p := newFake("p", []Result{{Title: "A"}}, nil)
	r := NewRouter(RouterOptions{Providers: []Provider{p}, MinInterval: time.Millisecond})
	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := r.Search(context.Background(), "go", 0)
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 8 {
		t.Fatalf("calls = %d, want 8", p.calls)
	}
}
