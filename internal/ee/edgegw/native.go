// edgegw/native.go fronts the native-client credential surface (the
// DPoP device-bound clients, design §6.6): activation proxies to the
// core's machine channel, while session-authenticated requests carry
// RFC 9449 proofs signed at the EDGE origin — so the edge verifies them
// here (machine introspection + proof check) and the validated caller
// travels the tunnel as an assertion, exactly like a browser session.
package edgegw

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/platform/httpx"
)

// validateClientAPI is the per-request credential fork for /api/*
// traffic: the Authorization scheme selects the path — Bearer/cookie is
// the browser session (authcore introspection), DPoP is the native
// device session (tenant introspection + RFC 9449 proof at this
// origin). Both resolve to the same tunnel assertion.
func (e *Edge) validateClientAPI(r *http.Request) (ActorAssertion, error) {
	if nc := nativeCredentialOf(r); nc.token != "" {
		assert, _, err := e.validateNative(r, nc)
		return assert, err
	}
	return e.validateClient(r.Context(), clientCredential(r))
}

// nativeVerifier checks DPoP proofs against the edge origin (the htu the
// native client signed is the edge URL, so verification belongs here).
var nativeVerifier = dpop.NewVerifier()

// nativeCredential is the Authorization/DPoP header pair of one request.
type nativeCredential struct {
	token string
	proof string
}

// nativeCredentialOf parses "Authorization: DPoP <session>" plus the
// DPoP proof header. A Bearer/cookie credential is NOT native — the
// browser path keeps its own extraction.
func nativeCredentialOf(r *http.Request) nativeCredential {
	h := r.Header.Get("Authorization")
	const prefix = "DPoP "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return nativeCredential{token: strings.TrimSpace(h[len(prefix):]), proof: r.Header.Get("DPoP")}
	}
	return nativeCredential{}
}

// nativeActivateLimiter bounds activation attempts per public caller —
// the edge's own limiter, because the machine channel behind it trusts
// the edge (its address would throttle every client collectively).
type nativeActivateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	max     int
	buckets map[string]int
	stamp   time.Time
}

func (l *nativeActivateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stamp.IsZero() || now.Sub(l.stamp) >= l.window {
		l.buckets = map[string]int{}
		l.stamp = now
	}
	l.buckets[key]++
	return l.buckets[key] <= l.max
}

// handleNativeActivate proxies the one-time key exchange to the core
// (the key travels this hop exactly once and is never cached or logged).
func (e *Edge) handleNativeActivate(w http.ResponseWriter, r *http.Request) {
	if !e.nativeLimiter.allow(r.RemoteAddr, time.Now()) {
		writeErr(w, http.StatusTooManyRequests, httpx.CodeRateLimited, "too many attempts")
		return
	}
	var req struct {
		APIKey   string          `json:"api_key"`
		DeviceID string          `json:"device_id"`
		DPoPJWK  json.RawMessage `json:"dpop_jwk"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "invalid request")
		return
	}
	if req.APIKey == "" || req.DeviceID == "" || len(req.DPoPJWK) == 0 {
		writeErr(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "invalid request")
		return
	}
	var out struct {
		SessionID string `json:"session_id"`
		ExpiresAt int64  `json:"expires_at"`
		DeviceID  string `json:"device_id"`
		Error     string `json:"error"`
	}
	status, err := e.coreAuthPOST(r.Context(), "/gw/native/activate", map[string]any{
		"api_key": req.APIKey, "device_id": req.DeviceID, "dpop_jwk": req.DPoPJWK,
	}, &out)
	if err != nil || status != http.StatusOK {
		e.writeNativeCoreFailure(w, status, err)
		return
	}
	if out.SessionID == "" {
		writeErr(w, http.StatusInternalServerError, httpx.CodeInternal, "activation unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"session_id": out.SessionID, "expires_at": out.ExpiresAt, "device_id": out.DeviceID,
	})
}

// handleNativeSession answers the caller's own session: the DPoP proof
// is verified at the edge, the fields come from the core introspection.
func (e *Edge) handleNativeSession(w http.ResponseWriter, r *http.Request) {
	nc := nativeCredentialOf(r)
	assert, info, err := e.validateNative(r, nc)
	if err != nil {
		e.writeNativeValidationFailure(w, err)
		return
	}
	_ = assert
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"expires_at":        info.ExpiresAt,
		"device_id":         info.DeviceID,
		"last_validated_at": info.LastValidatedAt,
	})
}

// handleNativeLogout revokes the DPoP session at the core after the
// proof check (possession precedes revocation — a stolen token without
// the device key cannot log its victim out).
func (e *Edge) handleNativeLogout(w http.ResponseWriter, r *http.Request) {
	nc := nativeCredentialOf(r)
	_, _, err := e.validateNative(r, nc)
	if err != nil {
		e.writeNativeValidationFailure(w, err)
		return
	}
	var out map[string]any
	status, err := e.coreAuthPOST(r.Context(), "/gw/native/revoke", map[string]string{"token": nc.token}, &out)
	if err != nil || status != http.StatusOK {
		e.writeNativeCoreFailure(w, status, err)
		return
	}
	e.nativeAuth.drop(nc.token)
	w.WriteHeader(http.StatusNoContent)
}

// nativeIntrospect is the core's machine-channel answer for one native
// session (the same shape /gw/native/introspect returns).
type nativeIntrospect struct {
	Active          bool   `json:"active"`
	Code            string `json:"code"`
	TenantID        string `json:"tenant_id"`
	UserID          string `json:"user_id"`
	Role            string `json:"role"`
	ExpiresAt       int64  `json:"expires_at"`
	DeviceID        string `json:"device_id"`
	DPoPThumbprint  string `json:"dpop_thumbprint"`
	LastValidatedAt int64  `json:"last_validated_at"`
}

// validateNative resolves one DPoP credential to the tunnel assertion
// plus the session facts. The proof is checked on EVERY request (that is
// the point of DPoP); the core introspection may be served from the
// anti-jitter cache and, when the core is unreachable, degrades along
// the last known validation — the browser session policy verbatim.
func (e *Edge) validateNative(r *http.Request, nc nativeCredential) (ActorAssertion, nativeIntrospect, error) {
	if nc.token == "" || nc.proof == "" {
		return ActorAssertion{}, nativeIntrospect{}, errInvalidCredential
	}
	req := dpop.Request{Method: r.Method, URI: edgeHTU(r)}
	now := time.Now()
	info, ok := e.nativeAuth.get(nc.token)
	if !ok || now.Sub(info.fetchedAt) > authCacheTTL {
		fresh, err := e.coreNativeIntrospect(r.Context(), nc.token)
		if err != nil {
			// Core unreachable: degrade along the last known validation.
			if cached, cok := e.nativeAuth.get(nc.token); cok && now.UnixMilli() <= cached.info.ExpiresAt {
				info = cached
			} else {
				return ActorAssertion{}, nativeIntrospect{}, errCoreAuthDown
			}
		} else {
			info = nativeAuthEntry{info: fresh, fetchedAt: now}
			if fresh.Active {
				e.nativeAuth.put(nc.token, info)
			} else {
				e.nativeAuth.drop(nc.token)
				if fresh.Code == "revoked" {
					return ActorAssertion{}, nativeIntrospect{}, errRevokedCredential
				}
				return ActorAssertion{}, nativeIntrospect{}, errInvalidCredential
			}
		}
	}
	if now.UnixMilli() > info.info.ExpiresAt {
		return ActorAssertion{}, nativeIntrospect{}, errInvalidCredential
	}
	if err := nativeVerifier.Verify(nc.proof, req, nc.token, info.info.DPoPThumbprint); err != nil {
		return ActorAssertion{}, nativeIntrospect{}, errInvalidCredential
	}
	return ActorAssertion{
		TenantID: info.info.TenantID,
		UserID:   info.info.UserID,
		Role:     info.info.Role,
	}, info.info, nil
}

func (e *Edge) coreNativeIntrospect(ctx context.Context, token string) (nativeIntrospect, error) {
	var out nativeIntrospect
	status, err := e.coreAuthPOST(ctx, "/gw/native/introspect", map[string]string{"token": token}, &out)
	if err != nil {
		return out, errCoreAuthDown
	}
	if status != http.StatusOK {
		return out, errCoreAuthDown
	}
	return out, nil
}

// edgeHTU is the htu the native client signed: the edge origin as the
// request reached it (TLS-aware, proxy-scheme honored), no query.
func edgeHTU(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v == "https" || v == "http" {
		scheme = v
	}
	host := strings.ToLower(r.Host)
	return scheme + "://" + host + r.URL.EscapedPath()
}

// nativeAuthEntry caches the last core introspection of one native
// token (assertion facts + the DPoP thumbprint proofs verify against).
type nativeAuthEntry struct {
	info      nativeIntrospect
	fetchedAt time.Time
}

type nativeAuthCache struct {
	mu sync.Mutex
	m  map[string]nativeAuthEntry
}

func (c *nativeAuthCache) get(token string) (nativeAuthEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[token]
	return e, ok
}

func (c *nativeAuthCache) drop(token string) {
	c.mu.Lock()
	delete(c.m, token)
	c.mu.Unlock()
}

func (c *nativeAuthCache) put(token string, e nativeAuthEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]nativeAuthEntry{}
	}
	c.m[token] = e
	now := time.Now().UnixMilli()
	for k, v := range c.m {
		if now > v.info.ExpiresAt {
			delete(c.m, k)
		}
	}
}

// writeNativeValidationFailure maps validation errors to the S-E
// contract (the same triple the browser path serves).
func (e *Edge) writeNativeValidationFailure(w http.ResponseWriter, err error) {
	switch {
	case err == errCoreAuthDown:
		writeErr(w, http.StatusServiceUnavailable, httpx.CodePlaneUnreachable, "private core unreachable")
	case err == errRevokedCredential:
		writeErr(w, http.StatusUnauthorized, httpx.CodeCredentialRevoked, "credential revoked")
	default:
		writeErr(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
	}
}

// writeNativeCoreFailure maps a failed machine-channel call to the S-E
// contract; a 401 from the core is an invalid key, the rest degrade as
// an unreachable core.
func (e *Edge) writeNativeCoreFailure(w http.ResponseWriter, status int, err error) {
	if err == nil && status == http.StatusUnauthorized {
		writeErr(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
		return
	}
	if err == nil && status == http.StatusBadRequest {
		writeErr(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "invalid request")
		return
	}
	writeErr(w, http.StatusServiceUnavailable, httpx.CodePlaneUnreachable, "private core unreachable")
}
