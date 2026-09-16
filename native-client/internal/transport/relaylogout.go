package transport

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// relayLogoutTimeout bounds the best-effort logout call — it must never
// stall the local logout.
const relayLogoutTimeout = 5 * time.Second

// RelayLogout performs POST /api/auth/logout against a woca-relay with the
// session token as Bearer (the relay accepts cookie OR Bearer; native has
// no cookie jar). It is the relay counterpart of the vault's best-effort
// remote revoke: the caller deletes the local token row regardless of the
// outcome. 2xx, 401, and 404 all count as success — the session is gone
// server-side either way.
func RelayLogout(ctx context.Context, client *http.Client, baseURL, token string) error {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, relayLogoutTimeout)
	defer cancel()
	// Canonical join, same as RelayLogin (trailing-slash URLs must never
	// produce "//api/auth/logout").
	endpoint := strings.TrimRight(baseURL, "/") + "/api/auth/logout"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("relay logout: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 ||
		resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("relay logout: unexpected status %d", resp.StatusCode)
}
