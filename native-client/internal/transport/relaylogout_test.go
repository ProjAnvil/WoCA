package transport

import (
	"net/http"
	"net/http/httptest"
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
