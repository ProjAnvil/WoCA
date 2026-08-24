// edgegw/auth.go is the edge's client-auth enforcement point (product
// design v1.8 §2.2/§2.5 R7): login and per-request session validation
// call home to the private core; the result is cached in-process for at
// most 60s (non-authoritative, anti-jitter) and — when the core is
// unreachable — an already-validated session keeps running until its
// natural expiry (monotone degradation; new identities fail closed).
// Validated callers travel to the bridge as an identity assertion on
// the authenticated tunnel; their credentials never reach the bridge.
package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookie is the edge-origin login cookie (the browser never
// persists the key; HttpOnly only).
const SessionCookie = "woca_session"

// authCacheTTL bounds the non-authoritative in-process cache (§2.5 R7).
// A variable so tests can shrink the window instead of waiting it out.
var authCacheTTL = 60 * time.Second

// errInvalidCredential and friends classify validation failures for the
// 401/503 contract.
var (
	errInvalidCredential = errors.New("edge: invalid credential")
	errRevokedCredential = errors.New("edge: revoked credential")
	errCoreAuthDown      = errors.New("edge: private core unreachable")
)

// bridgeActor is the identity assertion the bridge receives for
// tunneled traffic (the bridge's ActorAssertion mirrors this wire shape
// — the contract is deliberately duplicated across the two services,
// like every other cross-plane wire struct).
type bridgeActor struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Remark   string `json:"remark"`
	Role     string `json:"role"`
}

// actorFromBridge adapts the internal assertion to the wire shape.
func actorFromBridge(a *ActorAssertion) *bridgeActor {
	if a == nil {
		return nil
	}
	return &bridgeActor{TenantID: a.TenantID, UserID: a.UserID, Remark: a.Remark, Role: a.Role}
}

// authEntry is one cached validation result (never key material).
type authEntry struct {
	assert    ActorAssertion
	expiresAt int64 // the session's natural expiry (the degradation bound)
	fetchedAt time.Time
}

// authCache is the per-edge, in-memory, non-persistent validation cache.
type authCache struct {
	mu sync.Mutex
	m  map[string]authEntry // token → last validation
}

func (c *authCache) get(token string) (authEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[token]
	return e, ok
}

func (c *authCache) drop(token string) {
	c.mu.Lock()
	delete(c.m, token)
	c.mu.Unlock()
}

func (c *authCache) put(token string, e authEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]authEntry{}
	}
	c.m[token] = e
	// Bound the map: expired entries are dropped opportunistically.
	now := time.Now().UnixMilli()
	for k, v := range c.m {
		if now > v.expiresAt {
			delete(c.m, k)
		}
	}
}

// coreLoginResponse is the core's login answer (§2.1).
type coreLoginResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Remark    string `json:"remark"`
	Role      string `json:"role"`
	Code      string `json:"code,omitempty"` // failure classification
}

// coreLogin forwards a user login to the private core.
func (e *Edge) coreLogin(ctx context.Context, apiKey string) (coreLoginResponse, int, error) {
	var out coreLoginResponse
	status, err := e.coreAuthPOST(ctx, "/gw/auth/login", map[string]string{"api_key": apiKey}, &out)
	if err != nil {
		return out, 0, err
	}
	if status != http.StatusOK {
		return out, status, fmt.Errorf("core login: %d", status)
	}
	return out, status, nil
}

// coreLogout revokes the session at the core (best-effort).
func (e *Edge) coreLogout(ctx context.Context, token string) {
	var out map[string]any
	_, _ = e.coreAuthPOST(ctx, "/gw/auth/logout", map[string]string{"token": token}, &out)
}

// ActorAssertion is the edge's internal form of a validated caller.
type ActorAssertion struct {
	TenantID, UserID, Remark, Role string
}

// validateClient resolves a client credential (session token — cookie or
// Bearer) to the assertion the bridge will see. Fresh cache answers
// directly; stale-or-missing entries call home. On an unreachable core
// an already-validated session survives until its natural expiry
// (monotone degradation); anything else fails closed.
func (e *Edge) validateClient(ctx context.Context, token string) (ActorAssertion, error) {
	if token == "" {
		return ActorAssertion{}, errInvalidCredential
	}
	now := time.Now()
	if ent, ok := e.auth.get(token); ok {
		if now.UnixMilli() > ent.expiresAt {
			return ActorAssertion{}, errInvalidCredential
		}
		if now.Sub(ent.fetchedAt) <= authCacheTTL {
			return ent.assert, nil
		}
	}
	var resp struct {
		Active    bool   `json:"active"`
		Code      string `json:"code"`
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
		Remark    string `json:"remark"`
		Role      string `json:"role"`
		ExpiresAt int64  `json:"expires_at"`
	}
	status, err := e.coreAuthPOST(ctx, "/gw/auth/introspect", map[string]string{"token": token}, &resp)
	if err != nil {
		// Core unreachable: degrade along the last known validation.
		if ent, ok := e.auth.get(token); ok && now.UnixMilli() <= ent.expiresAt {
			return ent.assert, nil
		}
		return ActorAssertion{}, errCoreAuthDown
	}
	if status != http.StatusOK {
		if status == http.StatusServiceUnavailable || status == http.StatusBadGateway {
			return ActorAssertion{}, errCoreAuthDown
		}
		return ActorAssertion{}, errInvalidCredential
	}
	if !resp.Active {
		if resp.Code == "revoked" {
			// Drop the cached validation: a later outage must not serve a
			// revoked session along the stale-grace path.
			e.auth.drop(token)
			return ActorAssertion{}, errRevokedCredential
		}
		return ActorAssertion{}, errInvalidCredential
	}
	assert := ActorAssertion{TenantID: resp.TenantID, UserID: resp.UserID, Remark: resp.Remark, Role: resp.Role}
	e.auth.put(token, authEntry{assert: assert, expiresAt: resp.ExpiresAt, fetchedAt: now})
	return assert, nil
}

// coreAuthPOST is the call-home transport (the private network is the
// boundary, like every other /gw callback).
func (e *Edge) coreAuthPOST(ctx context.Context, path string, body any, out any) (int, error) {
	if e.cfg.ServiceURL == "" {
		return 0, errors.New("edge: no service plane configured")
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.ServiceURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("edge: core auth %s: %w", path, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(out); err != nil && resp.StatusCode == http.StatusOK {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// handleClientLogin intercepts the client login at the edge (§2.2): the
// core validates the key and issues the session; the browser gets the
// edge-origin HttpOnly cookie (and the bare token for native clients).
func (e *Edge) handleClientLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	out, status, err := e.coreLogin(r.Context(), req.APIKey)
	if err != nil && (status == 0 || status == http.StatusServiceUnavailable || status == http.StatusBadGateway) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "private core unreachable — login unavailable (fail-closed)"})
		return
	}
	if status != http.StatusOK {
		code := ""
		if strings.Contains(err.Error(), "revoked") || out.Code == "revoked" {
			code = "revoked"
		}
		body := map[string]string{"error": "unauthorized"}
		if code != "" {
			body["code"] = code
		}
		writeJSON(w, http.StatusUnauthorized, body)
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	maxAge := int(time.Until(time.UnixMilli(out.ExpiresAt)).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	// The login IS a validation: cache it, so an established session has
	// something to degrade along when the core goes dark (§2.1).
	e.auth.put(out.Token, authEntry{
		assert:    ActorAssertion{TenantID: out.TenantID, UserID: out.UserID, Remark: out.Remark, Role: out.Role},
		expiresAt: out.ExpiresAt,
		fetchedAt: time.Now(),
	})
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: out.Token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token": out.Token, "expires_at": out.ExpiresAt,
		"tenant_id": out.TenantID, "user_id": out.UserID,
		"remark": out.Remark, "role": out.Role,
	})
}

// handleClientLogout clears the edge cookie and revokes the session at
// the core (the authority).
func (e *Edge) handleClientLogout(w http.ResponseWriter, r *http.Request) {
	token := ""
	if c, err := r.Cookie(SessionCookie); err == nil {
		token = c.Value
	}
	if h := r.Header.Get("Authorization"); token == "" && len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		token = strings.TrimSpace(h[7:])
	}
	if token != "" {
		e.coreLogout(r.Context(), token)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// clientCredential extracts the session token from a client request
// (cookie first — EventSource — then Bearer).
func clientCredential(r *http.Request) string {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
