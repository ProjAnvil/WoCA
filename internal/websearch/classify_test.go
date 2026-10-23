package websearch

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyStatusRateLimited(t *testing.T) {
	cases := []struct {
		status int
		want   string // expected Reason substring
	}{
		{202, "challenge"}, // DDG answers throttled clients with 202 + JS challenge
		{429, "429"},
		{503, "503"},
	}
	for _, c := range cases {
		err := classifyStatus("p", c.status, 0)
		if err == nil {
			t.Fatalf("classifyStatus(%d) = nil, want rate-limit error", c.status)
		}
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("classifyStatus(%d) = %v, want ErrRateLimited", c.status, err)
		}
		if !Failover(err) {
			t.Errorf("classifyStatus(%d) should be failover-able", c.status)
		}
		rl, ok := errors.AsType[*RateLimitError](err)
		if !ok {
			t.Fatalf("classifyStatus(%d) = %T, want *RateLimitError", c.status, err)
		}
		if !strings.Contains(rl.Reason, c.want) {
			t.Errorf("classifyStatus(%d) Reason = %q, want to contain %q", c.status, rl.Reason, c.want)
		}
	}
}

func TestClassifyStatusNotRateLimited(t *testing.T) {
	// 400/401/403 are credential or request problems; 500/502/504 are backend
	// faults. None of them is "wait and try someone else", so none may trigger
	// a silent hop to another provider.
	for _, status := range []int{400, 401, 403, 404, 500, 502, 504} {
		err := classifyStatus("p", status, 0)
		if err == nil {
			t.Fatalf("classifyStatus(%d) = nil, want error", status)
		}
		if errors.Is(err, ErrRateLimited) {
			t.Errorf("classifyStatus(%d) = %v, must NOT be ErrRateLimited", status, err)
		}
		if Failover(err) {
			t.Errorf("classifyStatus(%d) must not trigger failover", status)
		}
		if !strings.Contains(err.Error(), "HTTP") {
			t.Errorf("classifyStatus(%d) = %q, want to mention HTTP", status, err)
		}
	}
}

func TestClassifyStatusSuccessIsNil(t *testing.T) {
	for _, status := range []int{200, 204} {
		if err := classifyStatus("p", status, 0); err != nil {
			t.Errorf("classifyStatus(%d) = %v, want nil", status, err)
		}
	}
}

func TestClassifyStatusRetryAfterPropagates(t *testing.T) {
	err := classifyStatus("p", 429, 30*time.Second)
	rl, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("classifyStatus(429) = %T, want *RateLimitError", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
}

func TestDetectAnomaly(t *testing.T) {
	blocked := []string{
		`<div class="anomaly-modal">Please complete the challenge</div>`,
		`<form id="challenge-form" action="/challenge">`,
		`<h1>Are you a robot?</h1>`,
		`<p>We have detected unusual traffic from your network</p>`,
		`<title>cf-browser-verification</title>`,
		`<p>If this error persists, please let us know</p>`,
		`<div class="cf-challenge-running">Checking your browser</div>`,
		`<p>Verify you are human to continue</p>`,
		`<noscript>Please enable JavaScript and cookies</noscript>`,
	}
	for _, body := range blocked {
		if !DetectAnomaly(body) {
			t.Errorf("DetectAnomaly(%s) = false, want true", truncate(body))
		}
	}

	clean := []string{
		`<html><body><a class="result__a" href="https://go.dev">Go</a></body></html>`,
		`<html><body>No results.</body></html>`,
		`<p>This page discusses verifying you are a human being</p>`,
		`<p>robot</p>`,
		"",
	}
	for _, body := range clean {
		if DetectAnomaly(body) {
			t.Errorf("DetectAnomaly(%s) = true, want false", truncate(body))
		}
	}
}

func TestDetectAnomalyIsCaseInsensitive(t *testing.T) {
	if !DetectAnomaly(`<div class="ANOMALY-MODAL">`) {
		t.Error("DetectAnomaly should ignore case")
	}
}

// TestDetectAnomalyNeedsPhraseNotWord guards the substring trap: a bare word
// marker like "anomaly" would misreport every empty search for "anomaly
// detection" as throttling.
func TestDetectAnomalyNeedsPhraseNotWord(t *testing.T) {
	for _, body := range []string{
		"Search for anomaly detection papers",
		"A challenge for modern form design",
	} {
		if DetectAnomaly(body) {
			t.Errorf("DetectAnomaly(%q) = true, want false", body)
		}
	}
}

func TestDetectAnomalyScansOnlyPrefix(t *testing.T) {
	// A marker past the scan cap is not seen; that is intentional (challenge
	// pages are small and front-loaded) and keeps cost bounded.
	body := strings.Repeat("x", anomalyScanBytes) + `<div class="anomaly-modal">`
	if DetectAnomaly(body) {
		t.Error("DetectAnomaly should only scan the bounded prefix")
	}
	if !DetectAnomaly(`<div class="anomaly-modal">` + strings.Repeat("x", anomalyScanBytes)) {
		t.Error("DetectAnomaly should find a marker inside the prefix")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{" 5 ", 5 * time.Second},
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	want := 2 * time.Hour
	when := time.Now().UTC().Add(want).Format(time.RFC1123)
	got := parseRetryAfter(when)
	if got <= 0 || got > want+time.Minute {
		t.Fatalf("parseRetryAfter(%q) = %v, want ~%v", when, got, want)
	}
}

func TestParseRetryAfterPastHTTPDate(t *testing.T) {
	when := time.Now().UTC().Add(-time.Hour).Format(time.RFC1123)
	if got := parseRetryAfter(when); got != 0 {
		t.Fatalf("parseRetryAfter(past date) = %v, want 0", got)
	}
}

func TestRateLimitErrorMessageIsActionable(t *testing.T) {
	a := &RateLimitError{Provider: "ddg-html", Reason: "HTTP 429"}
	if !strings.Contains(a.Error(), "do not retry or rephrase") {
		t.Errorf("message lost the directive: %q", a.Error())
	}
	if !strings.Contains(a.Error(), "ddg-html") {
		t.Errorf("message should name the provider: %q", a.Error())
	}
	// The bare sentinel must not gain a provider prefix.
	if ErrRateLimited.Error() != (&RateLimitError{}).Error() {
		t.Error("empty-provider RateLimitError should render exactly like ErrRateLimited")
	}
}

func TestFailoverOnlyOnRateLimit(t *testing.T) {
	cases := map[string]bool{
		"rate limit":  true,
		"other error": false,
		"no backend":  false,
	}
	errs := map[string]error{
		"rate limit":  &RateLimitError{Provider: "p"},
		"other error": errors.New("HTTP 500"),
		"no backend":  ErrNoBackend,
	}
	for name, err := range errs {
		if got := Failover(err); got != cases[name] {
			t.Errorf("Failover(%s) = %v, want %v", name, got, cases[name])
		}
	}
}

// TestFailoverSeesWrappedRateLimit: providers wrap errors with context, so the
// classification must survive wrapping.
func TestFailoverSeesWrappedRateLimit(t *testing.T) {
	wrapped := fmt.Errorf("search %q: %w", "go", &RateLimitError{Provider: "ddg-html"})
	if !Failover(wrapped) {
		t.Error("Failover must unwrap to find *RateLimitError")
	}
	if !errors.Is(wrapped, ErrRateLimited) {
		t.Error("errors.Is must reach ErrRateLimited through wrapping")
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
