// Package nativeauth implements the native-client credential surface (M2):
// activation exchanges an API key plus a device DPoP public key for an
// opaque session; validate reports the caller's session; logout revokes it.
// The activation route is exempt from the gateway's credential middleware
// (it bootstraps authentication, like an OAuth token endpoint) and carries
// its own attempt limiter.
//
// The surface implements server.NativeAuth — the enterprise seam the shared
// HTTP server exposes — over the tenant store and the DPoP verifier.
package nativeauth

import (
	"github.com/projanvil/woca/internal/platform/httpx"

	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/server"
)

// nativeSessionTTL is how long an activation lasts before the client must
// re-activate with a fresh API key.
const nativeSessionTTL = 30 * 24 * time.Hour

// Surface wires the tenant store and API-key provider behind the
// native-client endpoints. It implements server.NativeAuth.
type Surface struct {
	Store   *tenant.Store
	Keys    authn.Provider
	Now     func() time.Time
	limiter *attemptLimiter
}

// New returns the native-client credential surface over st and keys,
// rate-limiting activation attempts per caller.
func New(st *tenant.Store, keys authn.Provider) *Surface {
	return &Surface{Store: st, Keys: keys, limiter: newAttemptLimiter(10, time.Minute)}
}

// RegisterRoutes mounts the native endpoints on mux (server.NativeAuth).
func (na *Surface) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/native/activate", na.handleActivate)
	mux.HandleFunc("GET /api/native/session", na.handleSessionInfo)
	mux.HandleFunc("DELETE /api/native/session", na.handleLogout)
}

// SessionAuthenticator returns the DPoP session verifier for the gateway
// chain (server.NativeAuth).
func (na *Surface) SessionAuthenticator() gateway.SessionAuthenticator {
	return tenant.NewSessionProvider(na.Store, dpop.NewVerifier())
}

// compile-time check: the surface satisfies the server seam.
var _ server.NativeAuth = (*Surface)(nil)

type activateRequest struct {
	APIKey   string          `json:"api_key"`
	DeviceID string          `json:"device_id"`
	DPoPJWK  json.RawMessage `json:"dpop_jwk"`
}

type activateResponse struct {
	SessionID string `json:"session_id"`
	ExpiresAt int64  `json:"expires_at"`
	DeviceID  string `json:"device_id"`
}

// handleActivate exchanges an API key for a device-bound native session.
// Failures are uniform 401s (no oracle about which part failed); the body
// never echoes the API key.
func (na *Surface) handleActivate(w http.ResponseWriter, r *http.Request) {
	if !na.limiter.allow(r.RemoteAddr, na.now()) {
		writeNativeError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var req activateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.APIKey == "" || req.DeviceID == "" || len(req.DPoPJWK) == 0 {
		writeNativeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	thumbprint, err := dpop.ParseJWKThumbprint(req.DPoPJWK)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	tc, err := na.Keys.Authenticate(r.Context(), req.APIKey)
	if err != nil {
		writeNativeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// The session freezes the caller's scopes at activation; the tenancy
	// context's scope set cannot be reversed to a role name, so the member
	// role is pinned (admin scopes continue to flow through API keys).
	sess, full, err := na.Store.IssueSession(r.Context(), tc.TenantID, tc.UserID, req.DeviceID, thumbprint, nativeSessionRole(tc), nativeSessionTTL)
	if err != nil {
		writeNativeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(activateResponse{
		SessionID: full,
		ExpiresAt: sess.ExpiresAt,
		DeviceID:  sess.DeviceID,
	})
}

// nativeSessionRole derives the frozen role for a session from the
// authenticated context's scope set (superset wins).
func nativeSessionRole(tc tenancy.Context) string {
	scopes := tc.Scopes
	if hasAllScopes(scopes, authz.ScopesForRole(authz.RoleAdmin)) {
		return authz.RoleAdmin
	}
	if hasAllScopes(scopes, authz.ScopesForRole(authz.RoleMember)) {
		return authz.RoleMember
	}
	return authz.RoleViewer
}

// hasAllScopes reports whether want is contained in have.
func hasAllScopes(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, s := range have {
		set[s] = true
	}
	for _, s := range want {
		if !set[s] {
			return false
		}
	}
	return true
}

// handleSessionInfo reports the caller's own session. The request is
// already DPoP-authenticated; the access token is re-read from the header
// (verified moments earlier by the middleware).
func (na *Surface) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	token := nativeAccessToken(r)
	sess, ok, err := na.Store.LookupSession(r.Context(), token)
	if err != nil {
		writeNativeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeNativeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"expires_at":        sess.ExpiresAt,
		"device_id":         sess.DeviceID,
		"last_validated_at": sess.LastValidatedAt,
	})
}

// handleLogout revokes the caller's session (the opaque credential dies
// server-side; the client also deletes its local copy).
func (na *Surface) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := nativeAccessToken(r)
	if err := na.Store.RevokeSession(r.Context(), token); err != nil {
		writeNativeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNoContent)
}

func nativeAccessToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "DPoP "
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// writeNativeError emits the standard C-N envelope (api-standard.md §1):
// native-client-surface failures carry the client segment even when the
// server rejects the request.
func writeNativeError(w http.ResponseWriter, code int, msg string) {
	num := httpx.CodeInternal
	switch code {
	case http.StatusBadRequest:
		num = httpx.CodeInvalidRequest
	case http.StatusUnauthorized:
		num = httpx.CodeUnauthorized
	case http.StatusTooManyRequests:
		num = "0103"
	}
	httpx.WriteCoded(w, httpx.CodedError{Status: code, Segment: httpx.SegClientN, Code: num, Message: msg})
}

func (na *Surface) now() time.Time {
	if na.Now != nil {
		return na.Now()
	}
	return time.Now()
}

// attemptLimiter is a fixed-window per-key limiter over recent callers.
type attemptLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	max     int
	buckets map[string]int
	stamp   time.Time
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, buckets: make(map[string]int)}
}

func (l *attemptLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stamp.IsZero() || now.Sub(l.stamp) >= l.window {
		l.buckets = make(map[string]int)
		l.stamp = now
	}
	l.buckets[key]++
	return l.buckets[key] <= l.max
}
