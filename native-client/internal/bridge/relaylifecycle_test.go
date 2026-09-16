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
