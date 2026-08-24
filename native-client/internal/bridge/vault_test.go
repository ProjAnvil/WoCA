package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
)

type fakeActivator struct {
	result auth.ActivationResult
	err    error
	calls  int
}

func (f *fakeActivator) Activate(context.Context, auth.ActivationRequest) (auth.ActivationResult, error) {
	f.calls++
	return f.result, f.err
}

func openVaultStore(t *testing.T) *configstore.Store {
	t.Helper()
	s, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newVault(t *testing.T, st *configstore.Store) *DeviceSessionVault {
	t.Helper()
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewDeviceSessionVault(st, material, nil)
}

// newRevokingVault serves the BFF logout endpoint at a loopback test server
// and returns the vault plus the enterprise URL to connect to.
func newRevokingVault(t *testing.T, st *configstore.Store, revokePath *atomic.Pointer[string]) (*DeviceSessionVault, string) {
	t.Helper()
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		revokePath.Store(&p)
		if auth := r.Header.Get("Authorization"); auth == "" || r.Header.Get("DPoP") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return NewDeviceSessionVault(st, material, server.Client()), server.URL
}

func TestConnectPersistsSessionAndBootstrapReopens(t *testing.T) {
	st := openVaultStore(t)
	vault := newVault(t, st)
	activator := &fakeActivator{result: auth.ActivationResult{
		Ready: true, SessionID: "wsess_persisted", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}}
	svc := NewService(st, activator, WithSessionVault(vault))

	state, err := svc.Connect(context.Background(), ConnectionInput{
		ServerURL: "https://enterprise.example.com",
		APIKey:    "woca_test_key",
	})
	if err != nil || state.State != StateReady {
		t.Fatalf("Connect = %+v err=%v", state, err)
	}

	// A fresh service (process restart) reopens from the stored session.
	restarted := NewService(st, &fakeActivator{}, WithSessionVault(vault))
	boot, err := restarted.Bootstrap(context.Background())
	if err != nil || boot.State != StateReady {
		t.Fatalf("Bootstrap after restart = %+v err=%v", boot, err)
	}
	if boot.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("url = %q", boot.ServerURL)
	}
}

func TestExpiredSessionRequiresAuthorization(t *testing.T) {
	st := openVaultStore(t)
	vault := newVault(t, st)
	activator := &fakeActivator{result: auth.ActivationResult{
		Ready: true, SessionID: "wsess_expired", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(),
	}}
	svc := NewService(st, activator, WithSessionVault(vault))
	if _, err := svc.Connect(context.Background(), ConnectionInput{
		ServerURL: "https://enterprise.example.com", APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewService(st, &fakeActivator{}, WithSessionVault(vault))
	boot, err := restarted.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if boot.State != StateAuthorizationNeeded {
		t.Fatalf("expired session state = %s", boot.State)
	}
}

func TestLogoutRevokesRemotelyAndDeletesLocally(t *testing.T) {
	st := openVaultStore(t)
	var revokePath atomic.Pointer[string]
	vault, bffURL := newRevokingVault(t, st, &revokePath)
	activator := &fakeActivator{result: auth.ActivationResult{
		Ready: true, SessionID: "wsess_logout", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}}
	svc := NewService(st, activator, WithSessionVault(vault))
	if _, err := svc.Connect(context.Background(), ConnectionInput{
		ServerURL: bffURL, APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := svc.Logout(context.Background())
	if err != nil || out.State != StateAuthorizationNeeded {
		t.Fatalf("Logout = %+v err=%v", out, err)
	}
	if p := revokePath.Load(); p == nil || *p != "/api/native/session" {
		t.Fatalf("revoke path = %v", p)
	}
	restarted := NewService(st, &fakeActivator{}, WithSessionVault(vault))
	boot, _ := restarted.Bootstrap(context.Background())
	if boot.State != StateAuthorizationNeeded {
		t.Fatalf("session survived logout: %s", boot.State)
	}
}

func TestVaultSealedSessionNeverStoresPlaintext(t *testing.T) {
	st := openVaultStore(t)
	vault := newVault(t, st)
	profile, err := st.SaveProfile(context.Background(), "https://enterprise.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Save(context.Background(), profile.ID, "wsess_secret_value", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if id, ok := vault.Load(context.Background(), profile.ID); !ok || id != "wsess_secret_value" {
		t.Fatalf("Load = %q ok=%v", id, ok)
	}
	sess, ok, err := st.NativeSession(context.Background(), profile.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"wsess_secret_value", "secret_value"} {
		if contains, _ := bytesContains(sess.SessionCiphertext, forbidden); contains {
			t.Fatalf("ciphertext contains %q", forbidden)
		}
	}
	_ = dpop.NewVerifier
}

func bytesContains(b []byte, sub string) (bool, error) {
	return len(sub) == 0 || (len(b) >= len(sub) && indexOf(b, sub) >= 0), nil
}

func indexOf(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if b[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
