package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/projanvil/woca/native-client/internal/configstore"
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

// relayTokenStore wraps a real store and injects failures into the
// relay-token and bridge-pin mutations.
type relayTokenStore struct {
	nativeStore
	saveProfileErr      error
	activeProfileErr    error
	setBridgeErr        error
	setRelayTokenErr    error
	deleteRelayTokenErr error
}

func (s relayTokenStore) SaveProfile(ctx context.Context, rawURL string) (configstore.Profile, error) {
	if s.saveProfileErr != nil {
		return configstore.Profile{}, s.saveProfileErr
	}
	return s.nativeStore.SaveProfile(ctx, rawURL)
}

func (s relayTokenStore) ActiveProfile(ctx context.Context) (configstore.Profile, bool, error) {
	if s.activeProfileErr != nil {
		return configstore.Profile{}, false, s.activeProfileErr
	}
	return s.nativeStore.ActiveProfile(ctx)
}

func (s relayTokenStore) SetProfileBridge(ctx context.Context, baseURL, bridgeID string) error {
	if s.setBridgeErr != nil {
		return s.setBridgeErr
	}
	return s.nativeStore.SetProfileBridge(ctx, baseURL, bridgeID)
}

func (s relayTokenStore) SetProfileRelayToken(ctx context.Context, baseURL, token string) error {
	if s.setRelayTokenErr != nil {
		return s.setRelayTokenErr
	}
	return s.nativeStore.SetProfileRelayToken(ctx, baseURL, token)
}

func (s relayTokenStore) DeleteProfileRelayToken(ctx context.Context, baseURL string) error {
	if s.deleteRelayTokenErr != nil {
		return s.deleteRelayTokenErr
	}
	return s.nativeStore.DeleteProfileRelayToken(ctx, baseURL)
}

func TestConnectRelayRequiresStore(t *testing.T) {
	svc := NewService(nil, nil)
	_, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err == nil || !strings.Contains(err.Error(), "store is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectRelayBadProfileURL(t *testing.T) {
	svc := NewService(openTestStore(t), nil)
	_, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "://bad-url", APIKey: "wcr_key"})
	if err == nil {
		t.Fatal("unnormalizable URL accepted")
	}
}

func TestConnectRelaySetBridgeFailure(t *testing.T) {
	st := openTestStore(t)
	boom := errors.New("store write failed")
	svc := NewService(relayTokenStore{nativeStore: st, setBridgeErr: boom}, nil)
	_, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestConnectRelayTokenStoreFailure(t *testing.T) {
	st := openTestStore(t)
	// The login succeeded, but persisting the session token fails: the
	// profile stays unauthorized and the error surfaces.
	svc := NewService(relayTokenStore{nativeStore: st, setRelayTokenErr: errors.New("db full")}, nil,
		WithRelayLogin(relayLoginStub("sess-1")))
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err == nil {
		t.Fatal("token store failure swallowed")
	}
	if state.State != StateAuthorizationNeeded {
		t.Fatalf("state: %+v, want StateAuthorizationNeeded", state)
	}
}

func TestConnectRelayRejectsEmptySessionToken(t *testing.T) {
	// A login stub returning an empty token trips SetProfileRelayToken's
	// validation — the relay contract violation surfaces as an error.
	st := openTestStore(t)
	svc := NewService(st, nil, WithRelayLogin(relayLoginStub("")))
	_, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err == nil {
		t.Fatal("empty session token accepted")
	}
}
