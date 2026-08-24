// Package consoleauth is the shared credential gate for the private
// core's operator consoles (product design v1.7 §6.7): a username/password
// pair from the startup environment (WOCA_CONSOLE_USER /
// WOCA_CONSOLE_PASSWORD). Unset credentials leave the console open — the
// operator decided the network is the boundary (dev stacks, single-admin
// hosts). Configured credentials require a login exchange that mints a
// server-side session cookie. No keys to mint, no roles to manage — the
// consoles are single-admin surfaces by design.
package consoleauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// Session cookie and cadence.
const (
	SessionCookie = "woca_console_session"
	SessionTTL    = 12 * time.Hour
)

// Gate is the console's credential gate.
type Gate struct {
	user     string
	password string

	mu       sync.Mutex
	sessions map[string]time.Time // token → expires
}

// New reads the credential pair from the environment.
func New() *Gate {
	return &Gate{
		user:     strings.TrimSpace(os.Getenv("WOCA_CONSOLE_USER")),
		password: os.Getenv("WOCA_CONSOLE_PASSWORD"),
		sessions: map[string]time.Time{},
	}
}

// gated reports whether ANY credential env is set. A half-configured
// pair (exactly one of the two) is a misconfiguration: it must not
// silently degrade to an open console — the operator clearly intended a
// gate (and a single set value would leave the empty half matchable).
func (g *Gate) gated() bool { return g.user != "" || g.password != "" }

// Configured reports whether the login gate is fully and correctly set.
func (g *Gate) Configured() bool { return g.user != "" && g.password != "" }

// check validates presented credentials in constant time.
func (g *Gate) check(user, password string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(g.user))
	p := subtle.ConstantTimeCompare([]byte(password), []byte(g.password))
	return u&p == 1
}

// issue mints a session token.
func (g *Gate) issue() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	g.mu.Lock()
	g.sessions[token] = time.Now().Add(SessionTTL)
	g.sweepLocked()
	g.mu.Unlock()
	return token, nil
}

// valid checks (and slides) a session token.
func (g *Gate) valid(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(g.sessions, token)
		return false
	}
	g.sessions[token] = time.Now().Add(SessionTTL)
	return true
}

// sweepLocked drops expired sessions so un-presented tokens cannot
// accumulate (caller holds the mutex).
func (g *Gate) sweepLocked() {
	now := time.Now()
	for t, exp := range g.sessions {
		if now.After(exp) {
			delete(g.sessions, t)
		}
	}
}

func (g *Gate) drop(token string) {
	g.mu.Lock()
	delete(g.sessions, token)
	g.mu.Unlock()
}

// writeJSON keeps the wire envelope identical across consoles.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Login exchanges username/password for the session cookie. Anonymous by
// necessity — mounted outside the gate.
func (g *Gate) Login(w http.ResponseWriter, r *http.Request) {
	if !g.gated() {
		// No credentials set: nothing to log in to — the console is open.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "console login is not configured", "code": "console_open"})
		return
	}
	if !g.Configured() {
		// Half-configured pair: refuse — an open console is not what the
		// operator asked for, and the empty half would be matchable.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "console credentials misconfigured: set BOTH WOCA_CONSOLE_USER and WOCA_CONSOLE_PASSWORD (or neither for an open console)",
			"code":  "console_misconfigured",
		})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if !g.check(req.Username, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials", "code": "invalid_credentials"})
		return
	}
	token, err := g.issue()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Logout drops the calling session and clears the cookie.
func (g *Gate) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		g.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Context is the tenancy every authenticated (or open-mode) console
// request carries: the local platform's administrator. Per-route scope
// checks keep their meaning; the console session is the credential in
// front of them.
func Context() tenancy.Context {
	return tenancy.Context{
		TenantID: "local",
		UserID:   "console-admin",
		Scopes:   authz.ScopesForRole("tenant-admin"),
	}
}

// Wrap gates a console surface. The anonymous surface stays reachable so
// the gate can be bootstrapped from a browser: the SPA itself (non-API
// paths render the login form) and /api/health (infrastructure probes —
// R2's anonymous allowlist). Everything else requires the session cookie.
// Open when unconfigured; a half-configured credential pair refuses
// everything (fail closed — see gated()).
func (g *Gate) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anonymous := r.URL.Path == "/api/health" || !strings.HasPrefix(r.URL.Path, "/api/")
		if !g.gated() {
			if anonymous {
				h.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r.WithContext(tenancy.NewContext(r.Context(), Context())))
			return
		}
		if !g.Configured() {
			if anonymous {
				h.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "console credentials misconfigured: set BOTH WOCA_CONSOLE_USER and WOCA_CONSOLE_PASSWORD (or neither for an open console)",
				"code":  "console_misconfigured",
			})
			return
		}
		if anonymous {
			h.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(SessionCookie); err == nil && g.valid(c.Value) {
			h.ServeHTTP(w, r.WithContext(tenancy.NewContext(r.Context(), Context())))
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "console login required", "code": "console_login_required"})
	})
}
