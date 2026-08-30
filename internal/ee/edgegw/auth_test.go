package edgegw

// Tests for the edge as the auth enforcement point (product design v1.8
// §2.2/§2.5 R7): anonymous /api traffic bounces, login is answered at
// the edge (the core validates through the call-home), revocation
// propagates on the next validation, and a core outage keeps established
// sessions alive while new logins fail closed.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestEdgeAuthEnforcement(t *testing.T) {
	oldTTL := authCacheTTL
	authCacheTTL = 10 * time.Millisecond
	t.Cleanup(func() { authCacheTTL = oldTTL })
	parkPickupTimeout = 2 * time.Second
	defer func() { parkPickupTimeout = 30 * time.Second }()

	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_auth"))

	// Anonymous /api traffic bounces; the SPA root still serves.
	resp, err := http.Get(edge.URL + "/api/who")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous api = %d want 401", resp.StatusCode)
	}

	// Login is answered AT the edge: cookie + token, and the bridge sees
	// the assertion (the tunnel test covers the assertion ride).
	tok := userKey(t, edge, core, "alice")
	if !strings.HasPrefix(tok, "wocs_") {
		t.Fatalf("token = %q", tok)
	}

	// Revocation at the core propagates on the next validation: revoke
	// the key + sessions, let the cache window pass, expect 401 revoked.
	u, err := core.keys.UserByName(t.Context(), "local", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mustListKeys(t, core) {
		if k.UserID == u.ID && k.RevokedAt == 0 {
			if err := core.keys.RevokeKey(t.Context(), k.Prefix); err != nil {
				t.Fatal(err)
			}
			if err := core.auth.RevokeKeySessions(t.Context(), k.Prefix); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(authCacheTTL + 30*time.Millisecond)
	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || body.Code != "S-E-0002" {
		t.Fatalf("revoked through edge = %d code=%q", resp.StatusCode, body.Code)
	}
}

func TestEdgeCoreOutageGrace(t *testing.T) {
	oldTTL := authCacheTTL
	authCacheTTL = 10 * time.Millisecond
	t.Cleanup(func() { authCacheTTL = oldTTL })
	parkPickupTimeout = 2 * time.Second
	defer func() { parkPickupTimeout = 30 * time.Second }()

	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_grace"))

	tok := userKey(t, edge, core, "bob")

	// Fail the core's identity service: an established session keeps
	// working (monotone degradation), a new login fails closed.
	core.mu.Lock()
	core.authDown = true
	core.mu.Unlock()

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("established session during outage = %d want 200", resp.StatusCode)
	}

	resp2, err := http.Post(edge.URL+"/api/auth/login", "application/json", strings.NewReader(`{"api_key":"woca_anything"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new login during outage = %d want 503 (fail-closed)", resp2.StatusCode)
	}
}

func mustListKeys(t *testing.T, core *fakeCore) []struct {
	Prefix, UserID string
	RevokedAt      int64
} {
	t.Helper()
	rows, _, err := core.keys.ListKeys(t.Context(), "local", "", "", "", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]struct {
		Prefix, UserID string
		RevokedAt      int64
	}, 0, len(rows))
	for _, k := range rows {
		out = append(out, struct {
			Prefix, UserID string
			RevokedAt      int64
		}{k.Prefix, k.UserID, k.RevokedAt})
	}
	return out
}

var _ = zerolog.Nop
