package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/woca/native-client/internal/transport"
)

func TestConnectRelayStoresToken(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil, WithRelayLogin(func(_ context.Context, baseURL, apiKey string) (transport.RelaySession, error) {
		if baseURL != "https://relay.example.com" || apiKey != "wcr_key" {
			t.Errorf("login args: %q %q", baseURL, apiKey)
		}
		return transport.RelaySession{Token: "sess-1", ExpiresAt: 1700000000000}, nil
	}))
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "https://relay.example.com" {
		t.Fatalf("state: %+v", state)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "sess-1" {
		t.Fatalf("stored token: %q", got)
	}
	// The stored token re-opens the app after a restart (Bootstrap).
	restarted := NewService(st, nil)
	boot, err := restarted.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if boot.State != StateReady {
		t.Fatalf("bootstrap after restart: %+v, want StateReady", boot)
	}
}

func TestConnectRelayFailureKeepsProfileUnauthorized(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil, WithRelayLogin(func(context.Context, string, string) (transport.RelaySession, error) {
		return transport.RelaySession{}, &transport.RelayLoginError{Status: 401, Code: "revoked"}
	}))
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_dead"})
	if err == nil {
		t.Fatal("revoked key accepted")
	}
	var lerr *transport.RelayLoginError
	if !errors.As(err, &lerr) || lerr.Code != "revoked" {
		t.Fatalf("err %v, want RelayLoginError with code revoked", err)
	}
	if state.State != StateAuthorizationNeeded {
		t.Fatalf("state: %+v, want StateAuthorizationNeeded", state)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "" {
		t.Fatalf("token stored on failed login: %q", got)
	}
}

func TestConnectRelayRequiresKey(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil)
	if _, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com"}); err == nil {
		t.Fatal("empty key accepted")
	}
}
