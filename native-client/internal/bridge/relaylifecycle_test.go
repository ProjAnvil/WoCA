package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/transport"
)

// connectRelayProfile is the shared setup: a relay profile with a stored
// session token, ready to exercise the token lifecycle against.
func connectRelayProfile(t *testing.T, svc *Service, baseURL, token string) {
	t.Helper()
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: baseURL, APIKey: "wcr_key"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady {
		t.Fatalf("connect state: %+v, want StateReady", state)
	}
}

func relayLoginStub(token string) func(context.Context, string, string) (transport.RelaySession, error) {
	return func(context.Context, string, string) (transport.RelaySession, error) {
		return transport.RelaySession{Token: token, ExpiresAt: 1700000000000}, nil
	}
}

func TestLogoutClearsRelayToken(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil,
		WithRelayLogin(relayLoginStub("sess-1")))
	connectRelayProfile(t, svc, "https://relay.example.com", "sess-1")

	var logoutURL, logoutToken string
	svc.relayLogout = func(_ context.Context, baseURL, token string) error {
		logoutURL, logoutToken = baseURL, token
		return nil
	}
	state, err := svc.Logout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded {
		t.Fatalf("logout state: %+v, want StateAuthorizationNeeded", state)
	}
	// The local relay-token row must be gone — otherwise the next
	// Bootstrap silently re-opens with the still-server-valid token.
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "" {
		t.Fatalf("relay token survived logout: %q", got)
	}
	// The token was offered to the relay's logout endpoint first (best
	// effort server-side revocation, mirroring vault.Revoke).
	if logoutURL != "https://relay.example.com" || logoutToken != "sess-1" {
		t.Fatalf("relay logout call: %q %q", logoutURL, logoutToken)
	}
	// A restart must land on authorization_required, not ready.
	restarted := NewService(st, nil)
	boot, err := restarted.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if boot.State != StateAuthorizationNeeded {
		t.Fatalf("bootstrap after logout: %+v, want StateAuthorizationNeeded", boot)
	}
}

func TestLogoutDeletesRelayTokenEvenWhenRelayUnreachable(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil,
		WithRelayLogin(relayLoginStub("sess-1")),
		WithRelayLogout(func(context.Context, string, string) error {
			return errors.New("connection refused")
		}))
	connectRelayProfile(t, svc, "https://relay.example.com", "sess-1")

	if _, err := svc.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "" {
		t.Fatalf("relay token survived logout: %q", got)
	}
}

func TestConnectClearsStaleRelayToken(t *testing.T) {
	st := openTestStore(t)
	relaySvc := NewService(st, nil, WithRelayLogin(relayLoginStub("sess-1")))
	connectRelayProfile(t, relaySvc, "https://relay.example.com", "sess-1")

	// The same URL is later connected via the direct-bridge path: the
	// stale relay token must not shadow the fresh bridge credentials
	// (main.go's credential chain prefers the relay token).
	svc := NewService(st, &recordingActivator{result: auth.ActivationResult{Ready: true}})
	state, err := svc.Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "woca_bridge_key"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady {
		t.Fatalf("connect state: %+v, want StateReady", state)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "" {
		t.Fatalf("stale relay token shadowed direct-bridge connect: %q", got)
	}
}

func TestConnectLoopbackClearsStaleRelayToken(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetProfileRelayToken(t.Context(), "http://127.0.0.1:8787", "sess-1"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, nil)
	if _, err := svc.ConnectLoopback(t.Context(), "http://127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	if got := st.ProfileRelayToken(t.Context(), "http://127.0.0.1:8787"); got != "" {
		t.Fatalf("stale relay token survived loopback connect: %q", got)
	}
}

func TestConnectRelayTokenDeleteFailureSurfaces(t *testing.T) {
	// The stale-token sweep failing in a direct-bridge Connect surfaces
	// the store error instead of activating against a shadowed credential
	// chain.
	boom := errors.New("delete failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, deleteRelayTokenErr: boom},
		&recordingActivator{result: auth.ActivationResult{Ready: true}})
	_, err := svc.Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "woca_bridge_key"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestConnectLoopbackRelayTokenDeleteFailureSurfaces(t *testing.T) {
	boom := errors.New("delete failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, deleteRelayTokenErr: boom}, nil)
	_, err := svc.ConnectLoopback(t.Context(), "http://127.0.0.1:8787")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestConnectLoopbackValidation(t *testing.T) {
	// A nil store is a configuration error, and a non-loopback URL is
	// rejected before any store write.
	svc := NewService(nil, nil)
	if _, err := svc.ConnectLoopback(t.Context(), "http://127.0.0.1:8787"); err == nil {
		t.Fatal("nil store accepted")
	}
	svc = NewService(openTestStore(t), nil)
	if _, err := svc.ConnectLoopback(t.Context(), "https://remote.example.com"); err == nil {
		t.Fatal("non-loopback URL accepted")
	}
}

func TestLogoutRelayTokenDeleteFailureSurfaces(t *testing.T) {
	// The remote revoke is best effort, but the local row delete failing
	// is a hard error — otherwise Bootstrap would silently re-open.
	boom := errors.New("delete failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, deleteRelayTokenErr: boom}, nil,
		WithRelayLogin(relayLoginStub("sess-1")),
		WithRelayLogout(func(context.Context, string, string) error { return nil }))
	connectRelayProfile(t, svc, "https://relay.example.com", "sess-1")

	_, err := svc.Logout(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	// The token row is still there (the delete failed), so a restart
	// would still resume — exactly why the error must surface.
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "sess-1" {
		t.Fatalf("token after failed delete: %q", got)
	}
}

func TestLogoutUnconfigured(t *testing.T) {
	// No active profile: logout is a no-op landing on unconfigured.
	svc := NewService(openTestStore(t), nil)
	state, err := svc.Logout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateUnconfigured {
		t.Fatalf("state: %+v, want StateUnconfigured", state)
	}
}

func TestConnectLoopbackStoreFailures(t *testing.T) {
	// SaveProfile and SetProfileBridge failures surface before the
	// relay-token sweep is reached.
	boom := errors.New("store write failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, saveProfileErr: boom}, nil)
	if _, err := svc.ConnectLoopback(t.Context(), "http://127.0.0.1:8787"); !errors.Is(err, boom) {
		t.Fatalf("save profile: err = %v, want %v", err, boom)
	}
	svc = NewService(relayTokenStore{nativeStore: st, setBridgeErr: boom}, nil)
	if _, err := svc.ConnectLoopback(t.Context(), "http://127.0.0.1:8787"); !errors.Is(err, boom) {
		t.Fatalf("set bridge: err = %v, want %v", err, boom)
	}
}

func TestLogoutActiveProfileFailureSurfaces(t *testing.T) {
	boom := errors.New("store read failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, activeProfileErr: boom}, nil)
	if _, err := svc.Logout(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestConnectValidationAndStoreFailures(t *testing.T) {
	// Input validation and the pre-activation store writes all surface
	// before the activator runs.
	if _, err := NewService(openTestStore(t), nil).Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com"}); err == nil {
		t.Fatal("empty API key accepted")
	}
	if _, err := NewService(nil, nil).Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "k"}); err == nil {
		t.Fatal("nil store accepted")
	}
	boom := errors.New("store write failed")
	st := openTestStore(t)
	svc := NewService(relayTokenStore{nativeStore: st, saveProfileErr: boom}, nil)
	if _, err := svc.Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "k"}); !errors.Is(err, boom) {
		t.Fatalf("save profile: err = %v, want %v", err, boom)
	}
	svc = NewService(relayTokenStore{nativeStore: st, setBridgeErr: boom}, nil)
	if _, err := svc.Connect(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "k"}); !errors.Is(err, boom) {
		t.Fatalf("set bridge: err = %v, want %v", err, boom)
	}
}
