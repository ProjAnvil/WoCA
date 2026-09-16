package relay

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
)

// SessionCookieName is the client-session cookie (design §4.4).
const SessionCookieName = "woca_relay_session"

// loginAttemptsPerMinute bounds /api/auth/login per source IP (design §4.4).
const loginAttemptsPerMinute = 10

// AuthService owns the client-auth surface: login (rate-limited), logout,
// me, and session resolution for the forwarding plane. It is the relay's
// ONLY authentication line of defense (design §7.1).
type AuthService struct {
	keys        *KeyStore
	limiter     *RateLimiter
	behindProxy bool
	log         zerolog.Logger
}

// NewAuthService builds the auth surface; behindProxy is the explicit
// X-Forwarded-* trust switch (design §7.3).
func NewAuthService(keys *KeyStore, behindProxy bool, log zerolog.Logger) *AuthService {
	return &AuthService{
		keys:        keys,
		limiter:     NewRateLimiter(loginAttemptsPerMinute, 1024),
		behindProxy: behindProxy,
		log:         log,
	}
}

type loginRequest struct {
	APIKey string `json:"api_key"`
}

// HandleLogin implements POST /api/auth/login (design §3.3/§4.4): the SPA
// posts {api_key} and receives the HttpOnly session cookie plus — in the
// same response body — the opaque token native clients present as Bearer
// (the edgegw precedent: cookie AND body token, edgegw/auth.go:254-259
// semantics).
func (a *AuthService) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ip := a.ClientIP(r)
	if !a.limiter.Allow(ip) {
		a.log.Warn().Str("ip", ip).Msg("relay: login rate limit exceeded")
		WriteError(w, http.StatusTooManyRequests, CodeUnauthorized, "too many attempts; try again later")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid request")
		return
	}
	key, err := a.keys.LookupKey(r.Context(), req.APIKey)
	if errors.Is(err, ErrKeyRevoked) {
		// The SPA's revoked notice keys off exactly this code (design §2).
		WriteError(w, http.StatusUnauthorized, CodeRevoked, "key revoked")
		return
	}
	if err != nil {
		a.log.Warn().Str("ip", ip).Msg("relay: login failed")
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid key")
		return
	}
	token, expiresAt, err := a.keys.CreateSession(r.Context(), key.ID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeUnauthorized, "session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.SecureCookie(r),
		MaxAge:   int(SessionMaxAge.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt.UnixMilli(),
		"user_id":    key.UserID(),
		"remark":     key.Name,
		"role":       "admin",
	})
}

// HandleLogout implements POST /api/auth/logout: cookie OR Bearer (design
// §3.3); logout is a server-side session delete.
func (a *AuthService) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if token := SessionToken(r); token != "" {
		_ = a.keys.DeleteSession(r.Context(), token)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// HandleMe implements GET /api/auth/me with the SPA's Me shape (design
// §3.3): user_id is the key's hash-derived stable id, remark the key
// name, role fixed admin.
func (a *AuthService) HandleMe(w http.ResponseWriter, r *http.Request) {
	sess, err := a.SessionFromRequest(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": sess.UserID,
		"remark":  sess.KeyName,
		"role":    "admin",
	})
}

// SessionToken extracts the session credential: cookie first (browser
// EventSource cannot set headers), then Bearer (native clients).
func SessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// SessionFromRequest resolves the request's session to its key. Errors:
// ErrSessionUnknown (401 unauthorized), ErrKeyRevoked (401 revoked).
func (a *AuthService) SessionFromRequest(r *http.Request) (Session, error) {
	token := SessionToken(r)
	if token == "" {
		return Session{}, ErrSessionUnknown
	}
	return a.keys.ResolveSession(r.Context(), token)
}

// writeAuthError maps session-resolution failures onto the wire.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrKeyRevoked) {
		WriteError(w, http.StatusUnauthorized, CodeRevoked, "key revoked")
		return
	}
	WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
}

// ClientIP is the rate-limit/audit source address (design §7.3): the TCP
// peer by default; X-Forwarded-For only under explicit --behind-proxy.
func (a *AuthService) ClientIP(r *http.Request) string {
	if a.behindProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if ip, _, _ := strings.Cut(fwd, ","); strings.TrimSpace(ip) != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SecureCookie decides the session cookie's Secure flag (design §7.3):
// direct TLS is always secure; X-Forwarded-Proto is trusted only under
// --behind-proxy — otherwise a client could forge proto:http and trick
// the relay into issuing a cookie that also travels plaintext.
func (a *AuthService) SecureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return a.behindProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
