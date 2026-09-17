package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRelayLogoutPostsBearerToken(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := RelayLogout(t.Context(), srv.Client(), srv.URL+"/", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/auth/logout" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sess-1" {
		t.Fatalf("authorization: %q", gotAuth)
	}
}

func TestRelayLogoutToleratesGoneSession(t *testing.T) {
	// The relay answers 204 even for an unknown/expired token, but a
	// defensive 401/404 must not fail the best-effort logout either.
	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		if err := RelayLogout(t.Context(), srv.Client(), srv.URL, "sess-1"); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		srv.Close()
	}
}

func TestRelayLogoutDefaultClient(t *testing.T) {
	// A nil client falls back to http.DefaultClient.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	if err := RelayLogout(t.Context(), nil, srv.URL, "sess-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayLogoutBadURL(t *testing.T) {
	if err := RelayLogout(t.Context(), nil, "http://\x7f.invalid", "sess-1"); err == nil {
		t.Fatal("expected request construction error")
	}
}

func TestRelayLogoutUnreachable(t *testing.T) {
	err := RelayLogout(t.Context(), http.DefaultClient, "http://127.0.0.1:1", "sess-1")
	if err == nil || !strings.Contains(err.Error(), "relay logout:") {
		t.Fatalf("err = %v, want transport error", err)
	}
}

func TestRelayLogoutUnexpectedStatus(t *testing.T) {
	// A 5xx means the session may still live server-side — surfaced as an
	// error (the caller still deletes the local row regardless).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	err := RelayLogout(t.Context(), srv.Client(), srv.URL, "sess-1")
	if err == nil || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("err = %v, want unexpected-status error", err)
	}
}
