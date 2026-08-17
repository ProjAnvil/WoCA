package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchEndpointMatrix(t *testing.T) {
	ctx := context.Background()

	mk := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "raw-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}

	// Success with every window kind plus an unknown type that is skipped.
	pct := 42.0
	srv := mk(`{"success":true,"data":{"level":"pro","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42,"nextResetTime":1755432000000},
		{"type":"TOKENS_LIMIT","unit":6,"number":1,"nextResetTime":1755432000000},
		{"type":"TIME_LIMIT","unit":2,"percentage":`+jsonNum(pct)+`,"nextResetTime":1755432000000},
		{"type":"MYSTERY","unit":9}
	]}}`, http.StatusOK)
	info, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || !info.Available || info.Level != "pro" {
		t.Fatalf("success = %+v %v", info, err)
	}
	if len(info.Windows) != 3 {
		t.Fatalf("windows = %+v", info.Windows)
	}
	if info.WindowOf("5h") == nil || info.WindowOf("weekly") == nil || info.WindowOf("mcp") == nil {
		t.Fatalf("kinds = %+v", info.Windows)
	}
	srv.Close()

	// Limits present but none matching a known kind.
	srv = mk(`{"success":true,"data":{"level":"pro","limits":[{"type":"MYSTERY"}]}}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("unknown kinds = %+v %v", info, err)
	}
	srv.Close()

	// success=false with and without a message.
	srv = mk(`{"success":false,"msg":"rate limited"}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Available || info.Reason != "rate limited" {
		t.Fatalf("msg = %+v %v", info, err)
	}
	srv.Close()
	srv = mk(`{"success":false}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("default reason = %+v %v", info, err)
	}
	srv.Close()

	// Non-200 and decode errors.
	srv = mk("", http.StatusBadGateway)
	if _, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil); err == nil {
		t.Fatal("expected status error")
	}
	srv.Close()
	srv = mk("not json", http.StatusOK)
	if _, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil); err == nil {
		t.Fatal("expected decode error")
	}
	srv.Close()

	// Transport failure.
	if _, err := fetchEndpoint(ctx, "http://127.0.0.1:1/e", "raw-key", nil); err == nil {
		t.Fatal("expected transport error")
	}
	// Bad endpoint URL.
	if _, err := fetchEndpoint(ctx, "http://[::1]:namedport", "raw-key", nil); err == nil {
		t.Fatal("expected url error")
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
