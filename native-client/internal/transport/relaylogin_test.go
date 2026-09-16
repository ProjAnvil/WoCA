package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayLoginSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.APIKey != "wcr_test" {
			t.Errorf("login body: api_key=%q err=%v", req.APIKey, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "sess-1", "expires_at": 1700000000000})
	}))
	t.Cleanup(srv.Close)
	// A trailing slash on the profile URL must not break the join.
	sess, err := RelayLogin(context.Background(), srv.Client(), srv.URL+"/", "wcr_test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token != "sess-1" || sess.ExpiresAt != 1700000000000 {
		t.Fatalf("session: %+v", sess)
	}
}

func TestRelayLoginRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"revoked","error":"key revoked"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := RelayLogin(context.Background(), srv.Client(), srv.URL, "wcr_dead")
	var lerr *RelayLoginError
	if !errors.As(err, &lerr) {
		t.Fatalf("err %v, want *RelayLoginError", err)
	}
	if lerr.Code != "revoked" || lerr.Status != http.StatusUnauthorized {
		t.Fatalf("login error: %+v", lerr)
	}
}

func TestRelayLoginUnreachable(t *testing.T) {
	// A transport failure is a plain error, never a RelayLoginError (the
	// UI must not show "key revoked" for an offline relay).
	_, err := RelayLogin(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "wcr_x")
	if err == nil {
		t.Fatal("expected an error")
	}
	var lerr *RelayLoginError
	if errors.As(err, &lerr) {
		t.Fatalf("transport failure misclassified as RelayLoginError: %v", err)
	}
}
