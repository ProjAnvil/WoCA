package websearch

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Statuses that mean "back off and try another backend".
//
// 202 is here because DuckDuckGo answers clients it does not trust with 202
// plus a JavaScript challenge body rather than a 429. Treating it as a plain
// "HTTP 202" failure gives the model nothing to act on and invites retries,
// which deepens the throttle.
const (
	statusChallenge = http.StatusAccepted           // 202
	statusTooMany   = http.StatusTooManyRequests    // 429
	statusUnavail   = http.StatusServiceUnavailable // 503
)

// isRateLimitStatus reports whether a status means "back off and try another
// backend". Exported for adapters that receive a bare status code, such as the
// generic JSON provider wrapping internal/searchapi.
func isRateLimitStatus(code int) bool {
	return code == statusChallenge || code == statusTooMany || code == statusUnavail
}

// classifyStatus turns an HTTP status into a search error, attributing it to
// provider.
//
// It returns nil for success statuses, a *RateLimitError for statuses that
// warrant failing over to another provider, and a plain error otherwise. The
// provider is recorded on the error because the router needs to know whom to
// put in the cooldown table.
func classifyStatus(provider string, status int, retryAfter time.Duration) error {
	switch status {
	case statusChallenge:
		return &RateLimitError{Provider: provider, RetryAfter: retryAfter, Reason: "HTTP 202 (anti-bot challenge)"}
	case statusTooMany:
		return &RateLimitError{Provider: provider, RetryAfter: retryAfter, Reason: "HTTP 429"}
	case statusUnavail:
		return &RateLimitError{Provider: provider, RetryAfter: retryAfter, Reason: "HTTP 503"}
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return fmt.Errorf("HTTP %d", status)
}

// anomalyMarkers identify anti-bot interstitials.
//
// Each marker is deliberately a phrase rather than a bare word: a search for
// "anomaly detection" can legitimately return a page whose text contains
// "anomaly", and misreading that as a block would report throttling for a
// genuine empty result.
var anomalyMarkers = []string{
	"anomaly-modal",
	"challenge-form",
	"cf-browser-verification",
	"cf-challenge",
	"captcha",
	"unusual traffic",
	"unusual activity",
	"are you a robot",
	"verify you are human",
	"enable javascript and cookies",
	"if this error persists",
}

// anomalyScanBytes caps how much of a body is scanned. Challenge pages are
// small and appear at the top of the document, so scanning a prefix is enough
// and keeps the cost bounded on multi-megabyte responses. The slice may land
// mid-rune, which is harmless here because the result is only used for
// substring matching, never for output.
const anomalyScanBytes = 64 << 10

// DetectAnomaly reports whether a response body that yielded zero results looks
// like an anti-bot interstitial rather than a genuine empty result set.
//
// It must only be consulted when a provider parsed zero results: markers are
// matched against page text, and a page that produced results is by definition
// not a block page.
func DetectAnomaly(body string) bool {
	if body == "" {
		return false
	}
	if len(body) > anomalyScanBytes {
		body = body[:anomalyScanBytes]
	}
	lower := strings.ToLower(body)
	for _, marker := range anomalyMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseRetryAfter reads a Retry-After header value, accepting both the
// delta-seconds and HTTP-date forms. It returns 0 when the value is absent,
// unparseable, or already in the past, letting callers fall back to a default
// cooldown.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// http.ParseTime only accepts the GMT spelling of RFC 1123, but servers in
	// the wild also emit "UTC". Try both before giving up.
	for _, parse := range []func(string) (time.Time, error){
		http.ParseTime,
		func(s string) (time.Time, error) { return time.Parse(time.RFC1123, s) },
	} {
		if when, err := parse(v); err == nil {
			if d := time.Until(when); d > 0 {
				return d
			}
			return 0
		}
	}
	return 0
}
