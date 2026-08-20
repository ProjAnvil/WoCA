// activator_http.go is the M2 production activator: it exchanges the API key
// for a device-bound opaque session at the BFF activation endpoint. The key
// travels exactly once, in the request body, and is never logged.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/projanvil/woca/native-client/internal/devicekey"
)

// HTTPActivator activates against a remote BFF using the device material.
type HTTPActivator struct {
	client   *http.Client
	material devicekey.Material
}

// NewHTTPActivator returns an activator over client (nil uses the default).
func NewHTTPActivator(client *http.Client, material devicekey.Material) *HTTPActivator {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPActivator{client: client, material: material}
}

type activateRequestBody struct {
	APIKey   string          `json:"api_key"`
	DeviceID string          `json:"device_id"`
	DPoPJWK  json.RawMessage `json:"dpop_jwk"`
}

type activateResponseBody struct {
	SessionID string `json:"session_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// ErrInvalidCredentials reports a rejected API key (HTTP 401).
var ErrInvalidCredentials = fmt.Errorf("native activation: invalid credentials")

// ErrActivationRateLimited reports the BFF's limiter (HTTP 429).
var ErrActivationRateLimited = fmt.Errorf("native activation: rate limited")

// Activate posts the one-time exchange and returns the issued session.
func (a *HTTPActivator) Activate(ctx context.Context, req ActivationRequest) (ActivationResult, error) {
	jwk, err := devicekey.PublicJWK(a.material.DPoPKey.PublicKey)
	if err != nil {
		return ActivationResult{}, err
	}
	body, err := json.Marshal(activateRequestBody{
		APIKey:   req.APIKey,
		DeviceID: a.material.DeviceID,
		DPoPJWK:  jwk,
	})
	if err != nil {
		return ActivationResult{}, err
	}
	url := req.BaseURL + "/api/native/activate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ActivationResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("native activation: connect: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized:
		return ActivationResult{}, ErrInvalidCredentials
	case resp.StatusCode == http.StatusTooManyRequests:
		return ActivationResult{}, ErrActivationRateLimited
	default:
		return ActivationResult{}, fmt.Errorf("native activation: status %d", resp.StatusCode)
	}
	var out activateResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ActivationResult{}, fmt.Errorf("native activation: response: %w", err)
	}
	if out.SessionID == "" {
		return ActivationResult{}, fmt.Errorf("native activation: empty session")
	}
	return ActivationResult{Ready: true, SessionID: out.SessionID, ExpiresAt: out.ExpiresAt}, nil
}
