package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RelaySession is the relay login's body-token contract (design §3.3:
// the login response carries the opaque session token the native client
// stores via SetProfileRelayToken and later presents as a StaticBearer
// credential).
type RelaySession struct {
	Token     string
	ExpiresAt int64
}

// RelayLoginError carries the relay's enumerated failure code ("revoked"
// is the frontend contract — the connect page surfaces it distinctly).
type RelayLoginError struct {
	Status int
	Code   string
}

func (e *RelayLoginError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("relay login failed (HTTP %d): %s", e.Status, e.Code)
	}
	return fmt.Sprintf("relay login failed (HTTP %d)", e.Status)
}

// RelayLogin performs POST /api/auth/login against a woca-relay with the
// wcr_ key and returns the body token. Transport failures are plain
// errors; HTTP failures decode the relay's {"code": ...} envelope into
// *RelayLoginError.
func RelayLogin(ctx context.Context, client *http.Client, baseURL, apiKey string) (RelaySession, error) {
	if client == nil {
		client = http.DefaultClient
	}
	// Canonical join: a trailing-slash profile URL must never produce
	// "//api/auth/login" (ServeMux 301s would re-issue the POST as GET).
	endpoint := strings.TrimRight(baseURL, "/") + "/api/auth/login"
	body, err := json.Marshal(map[string]string{"api_key": apiKey})
	if err != nil {
		return RelaySession{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RelaySession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return RelaySession{}, fmt.Errorf("relay login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&envelope)
		return RelaySession{}, &RelayLoginError{Status: resp.StatusCode, Code: envelope.Code}
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&out); err != nil {
		return RelaySession{}, fmt.Errorf("relay login: decode: %w", err)
	}
	if out.Token == "" {
		return RelaySession{}, fmt.Errorf("relay login: empty session token")
	}
	return RelaySession{Token: out.Token, ExpiresAt: out.ExpiresAt}, nil
}
