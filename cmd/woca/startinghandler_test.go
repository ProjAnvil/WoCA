package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startingHandler serves the pre-agent window of `woca serve`: /api/health
// answers so the webd readiness probe passes while the (network-bound)
// agent build is still running; everything else reports 503.
func TestStartingHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	startingHandler(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health during startup = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("health body = %q", body)
	}

	rec = httptest.NewRecorder()
	startingHandler(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-health API during startup = %d, want 503", rec.Code)
	}

	// The UI reloads itself until boot completes instead of showing a raw
	// 503 JSON page.
	rec = httptest.NewRecorder()
	startingHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("UI during startup = %d %q, want 200 with auto-refresh", rec.Code, rec.Body.String())
	}
}
