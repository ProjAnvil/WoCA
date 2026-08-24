// vault.go is the production SessionVault: sessions are AES-256-GCM sealed
// by the device key into the config store, and logout revokes the session at
// the BFF with a DPoP-bound DELETE before the local copy is deleted.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
)

// DeviceSessionVault persists sealed native sessions in the config store.
type DeviceSessionVault struct {
	store    *configstore.Store
	material devicekey.Material
	client   *http.Client
	now      func() time.Time
}

// NewDeviceSessionVault returns the vault over st. A nil client uses
// http.DefaultClient for the logout call.
func NewDeviceSessionVault(st *configstore.Store, material devicekey.Material, client *http.Client) *DeviceSessionVault {
	if client == nil {
		client = http.DefaultClient
	}
	return &DeviceSessionVault{store: st, material: material, client: client, now: time.Now}
}

// Load returns the stored session id when it exists, decrypts, and is
// unexpired.
func (v *DeviceSessionVault) Load(ctx context.Context, profileID int64) (string, bool) {
	stored, ok, err := v.store.NativeSession(ctx, profileID)
	if err != nil || !ok {
		return "", false
	}
	sessionID, expiresAt, err := devicekey.OpenSession(v.material, stored.SessionCiphertext)
	if err != nil || sessionID == "" {
		return "", false
	}
	if expiresAt <= v.now().UnixMilli() {
		return "", false
	}
	_ = v.store.TouchNativeSession(ctx, profileID, v.now().UnixMilli())
	return sessionID, true
}

// Save seals and persists a freshly activated session.
func (v *DeviceSessionVault) Save(ctx context.Context, profileID int64, sessionID string, expiresAt int64) error {
	sealed, err := devicekey.SealSession(v.material, sessionID, expiresAt)
	if err != nil {
		return err
	}
	return v.store.SaveNativeSession(ctx, profileID, v.material.DeviceID, sealed, expiresAt)
}

// Delete removes the local session.
func (v *DeviceSessionVault) Delete(ctx context.Context, profileID int64) error {
	return v.store.DeleteNativeSession(ctx, profileID)
}

// Revoke signs a DPoP proof for the logout endpoint and issues the DELETE.
// Failures are returned but callers treat revocation as best effort.
func (v *DeviceSessionVault) Revoke(ctx context.Context, baseURL, sessionID string) error {
	uri := baseURL + "/api/native/session"
	proof, err := dpop.Sign(v.material.DPoPKey, dpop.ProofInput{
		HTM:         http.MethodDelete,
		HTU:         uri,
		AccessToken: sessionID,
		IAT:         v.now(),
		JTI:         fmt.Sprintf("logout-%d", v.now().UnixNano()),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uri, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "DPoP "+sessionID)
	req.Header.Set("DPoP", proof)
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("native logout: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound {
		return errors.New("native logout: unexpected status")
	}
	return nil
}
