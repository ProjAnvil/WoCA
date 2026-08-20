package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/dpop"
	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
	"github.com/projanvil/woca/native-client/internal/transport"
)

// TestNativeAuthEndToEnd drives the real client credential stack (HTTP
// activator, DPoP credential provider, session vault) against a live
// enterprise gateway. Set WOCA_M2_E2E_URL (gateway base URL) and
// WOCA_M2_E2E_KEY (an issued API key) to run it:
//
//	./build/woca tenant create … && ./build/woca tenant issue …
//	./build/woca serve --auth apikey --db … --addr 127.0.0.1:8898 --no-browser
//	WOCA_M2_E2E_URL=http://127.0.0.1:8898 WOCA_M2_E2E_KEY=woca_… \
//	  go test -run TestNativeAuthEndToEnd . -v
func TestNativeAuthEndToEnd(t *testing.T) {
	baseURL := os.Getenv("WOCA_M2_E2E_URL")
	apiKey := os.Getenv("WOCA_M2_E2E_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("WOCA_M2_E2E_URL / WOCA_M2_E2E_KEY not set")
	}
	ctx := context.Background()

	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	vault := bridge.NewDeviceSessionVault(store, material, nil)
	service := bridge.NewService(store, auth.NewHTTPActivator(nil, material), bridge.WithSessionVault(vault))

	// 1. Connect activates over the wire and persists the sealed session.
	state, err := service.Connect(ctx, bridge.ConnectionInput{ServerURL: baseURL, APIKey: apiKey})
	if err != nil || state.State != bridge.StateReady {
		t.Fatalf("Connect = %+v err=%v", state, err)
	}

	// 2. The DPoP credential provider mints proofs the gateway accepts.
	credentials := transport.NewDPoPCredentials(material, func() (string, bool) {
		profile, ok, err := store.ActiveProfile(ctx)
		if err != nil || !ok {
			return "", false
		}
		return vault.Load(ctx, profile.ID)
	})
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/native/session", nil)
	creds, err := credentials.Credentials(ctx, http.MethodGet, req.URL)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", creds.Scheme+" "+creds.Session)
	req.Header.Set("DPoP", creds.Proof)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session validate = %d, want 200", resp.StatusCode)
	}

	// 3. Restart: a fresh service reopens from the sealed session.
	restarted := bridge.NewService(store, auth.NewHTTPActivator(nil, material), bridge.WithSessionVault(vault))
	boot, err := restarted.Bootstrap(ctx)
	if err != nil || boot.State != bridge.StateReady {
		t.Fatalf("Bootstrap after restart = %+v err=%v", boot, err)
	}

	// 4. Logout revokes remotely; the old session id no longer authenticates.
	// Capture the raw id before logout — the local copy is deleted by then.
	profile, _, err := store.ActiveProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldSession, ok := vault.Load(ctx, profile.ID)
	if !ok {
		t.Fatal("session vanished before logout")
	}
	out, err := restarted.Logout(ctx)
	if err != nil || out.State != bridge.StateAuthorizationNeeded {
		t.Fatalf("Logout = %+v err=%v", out, err)
	}
	if _, err := credentials.Credentials(ctx, http.MethodGet, req.URL); err == nil {
		t.Fatal("credentials still mint after logout")
	}
	// The revoked id itself must be rejected by the gateway.
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/api/native/session", nil)
	req2.Header.Set("Authorization", "DPoP "+oldSession)
	proof2, err := dpop.Sign(material.DPoPKey, dpop.ProofInput{
		HTM: http.MethodGet, HTU: baseURL + "/api/native/session",
		AccessToken: oldSession, IAT: time.Now(), JTI: "e2e-post-logout",
	})
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("DPoP", proof2)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout validate = %d, want 401", resp2.StatusCode)
	}
}
