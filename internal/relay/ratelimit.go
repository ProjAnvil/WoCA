package relay

import (
	"sync"
	"time"
)

// RateLimiter is a bounded per-key token bucket written against the
// standard library only (design §4.4: golang.org/x/time/rate is NOT in
// the personal dependency baseline and would fail check-deps). The entry
// map is capped and lazily evicted so a spray of source IPs cannot grow
// memory without bound.
type RateLimiter struct {
	mu         sync.Mutex
	rate       float64 // tokens per second
	burst      float64
	maxEntries int
	now        func() time.Time
	entries    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute tokens per minute per key (burst equal
// to perMinute) and keeps at most maxEntries live buckets.
func NewRateLimiter(perMinute float64, maxEntries int) *RateLimiter {
	return &RateLimiter{
		rate:       perMinute / 60,
		burst:      perMinute,
		maxEntries: maxEntries,
		now:        time.Now,
		entries:    map[string]*bucket{},
	}
}

// Allow consumes one token for key, reporting whether the call is within
// the limit.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, ok := l.entries[key]; !ok && len(l.entries) >= l.maxEntries {
		l.evictLocked(now)
	}
	b, ok := l.entries[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.entries[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictLocked drops buckets idle for more than ten minutes; when the map
// is still at capacity it trims arbitrary survivors down to 90% (map
// order is random — under a flood the cost is a temporarily wider limit
// for evicted IPs, the right trade under a DoS posture).
func (l *RateLimiter) evictLocked(now time.Time) {
	for k, b := range l.entries {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.entries, k)
		}
	}
	floor := l.maxEntries * 9 / 10
	for k := range l.entries {
		if len(l.entries) <= floor {
			return
		}
		delete(l.entries, k)
	}
}
