package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

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
