package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestDirectAssemblyServesOpsPaths pins the direct assembly's ops-path
// behavior end to end (the console API chain over a fresh database): the
// health route, an open-gate tenant route, and the skill-asset mount —
// the exact wiring the shared assembly (F4) must keep identical.
func TestDirectAssemblyServesOpsPaths(t *testing.T) {
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "ops.db")
	h := directAssembly(zerolog.Nop(), dsn)
	get := func(path string) (*http.Response, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Result(), rec.Body.String()
	}
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/health", `{"ok":true,"service":"woca-ops-console"}`},
		{"/api/tenant/users", `{"items":[],"total":0}`},
		{"/api/skills", `[]`},
	} {
		resp, body := get(tc.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", tc.path, resp.StatusCode)
		}
		if got := strings.TrimSpace(body); got != tc.body {
			t.Errorf("%s body = %q, want %q", tc.path, got, tc.body)
		}
	}
}

func TestProxyAssemblyForwardsManagementRequest(t *testing.T) {
	type forwardedRequest struct {
		host   string
		uri    string
		header http.Header
	}
	seen := make(chan forwardedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- forwardedRequest{host: r.Host, uri: r.RequestURI, header: r.Header.Clone()}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(upstream.Close)
	handler := proxyAssembly(zerolog.Nop(), upstream.URL+"/ignored-prefix")
	req := httptest.NewRequest(http.MethodGet, "http://console.example/api/items/a%2Fb?name=a%2Bb&flag=1", nil)
	req.RemoteAddr = "192.0.2.20:1234"
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Cookie", "session=test-session")
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	req.Header.Set("X-Forwarded-Host", "untrusted.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.String() != "proxied" {
		t.Fatalf("proxy response = %d %q", rec.Code, rec.Body.String())
	}
	got := <-seen
	if got.host != "console.example" || got.uri != "/api/items/a%2Fb?name=a%2Bb&flag=1" {
		t.Fatalf("forwarded host/URI = %q %q", got.host, got.uri)
	}
	for name, want := range map[string]string{
		"Authorization":     "Bearer test-token",
		"Cookie":            "session=test-session",
		"X-Forwarded-For":   "192.0.2.20",
		"X-Forwarded-Host":  "console.example",
		"X-Forwarded-Proto": "http",
	} {
		if value := got.header.Get(name); value != want {
			t.Errorf("upstream %s = %q, want %q", name, value, want)
		}
	}
}
