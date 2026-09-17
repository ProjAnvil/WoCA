package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRelayLoginErrorMessage(t *testing.T) {
	for _, c := range []struct {
		name string
		err  RelayLoginError
		want string
	}{
		{"with code", RelayLoginError{Status: 401, Code: "revoked"}, "relay login failed (HTTP 401): revoked"},
		{"without code", RelayLoginError{Status: 500}, "relay login failed (HTTP 500)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.Error(); got != c.want {
				t.Fatalf("Error() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRelayLoginDefaultClient(t *testing.T) {
	// A nil client falls back to http.DefaultClient (fine for plain-HTTP
	// test servers; production wires the TOFU-pinned client).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "sess-2", "expires_at": 1})
	}))
	t.Cleanup(srv.Close)
	sess, err := RelayLogin(context.Background(), nil, srv.URL, "wcr_test")
	if err != nil || sess.Token != "sess-2" {
		t.Fatalf("session = %+v, err = %v", sess, err)
	}
}

func TestRelayLoginBadURL(t *testing.T) {
	// A control character in the base URL fails request construction.
	if _, err := RelayLogin(context.Background(), nil, "http://\x7f.invalid", "wcr_x"); err == nil {
		t.Fatal("expected request construction error")
	}
}

func TestRelayLoginDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not-json"))
	}))
	t.Cleanup(srv.Close)
	_, err := RelayLogin(context.Background(), srv.Client(), srv.URL, "wcr_x")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func TestRelayLoginEmptyToken(t *testing.T) {
	// A 200 without a token is a contract violation, not a session.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "", "expires_at": 1})
	}))
	t.Cleanup(srv.Close)
	_, err := RelayLogin(context.Background(), srv.Client(), srv.URL, "wcr_x")
	if err == nil || !strings.Contains(err.Error(), "empty session token") {
		t.Fatalf("err = %v, want empty-token error", err)
	}
}
