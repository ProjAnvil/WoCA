// Package bridge implements the private-cloud bridge service (product
// design v1.8): the execution plane users hold themselves. The bridge
// carries ZERO user credentials — no key hashes, no user tables: logins
// are forwarded to the private core over the machine channel (§2.7
// introspection), sessions are issued and revoked there (§2.1), and
// scenario-four callers arrive as the edge's identity assertions (§2.2).
// A pure standalone bridge (not enrolled) keeps only loopback trust; the
// fail-closed bind check enforces that posture. The gateway channel
// (enrollment, config pull) lives in the gateway subpackage.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/projanvil/woca/internal/platform/httpx"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

// sessionTTL caps how long an established session survives without the
// core (the natural-expiry bound of the degradation model, §2.1).
const sessionTTL = 7 * 24 * time.Hour

// sessionScheme prefixes core-issued session tokens.
const sessionScheme = "wocs"

// recheckGap is the introspection freshness window: a mirrored session
// is re-validated against the core at most this often (≤60s, §2.7). A
// variable so tests can shrink the window instead of sleeping through it.
var recheckGap = 60 * time.Second

// Introspector is the bridge's channel-two view of the core identity
// service: the cloud-access channel provides it once enrolled.
type Introspector interface {
	// CoreLogin forwards a user login; a transport failure returns
	// ErrCoreUnreachable (the caller fails closed), ErrCoreLoginRevoked
	// carries the enumerated revoked code.
	CoreLogin(ctx context.Context, apiKey string) (CoreSession, error)
	// CoreIntrospect validates a session token. Inactive results are
	// data, not errors; transport failures return ErrCoreUnreachable
	// (the caller's grace decision point).
	CoreIntrospect(ctx context.Context, token string) (CoreSession, error)
	// CoreLogout revokes the session at the core (the authority).
	CoreLogout(ctx context.Context, token string) error
}

// Auth is the bridge identity surface: the core-session mirror with
// online re-validation, plus the edge's tunnel assertions. It implements
// authn.Provider and mounts the login/logout routes.
type Auth struct {
	sessions *sessionStore
	auditLog *Audit

	// introsp is set by the serve wiring once the cloud channel exists
	// (atomic: the channel (re)builds at runtime).
	mu        sync.RWMutex
	introspec Introspector
}

// NewAuth builds the bridge identity surface over the settings database.
func NewAuth(wdb *wocadb.DB) (*Auth, error) {
	sessions, err := newSessionStore(wdb)
	if err != nil {
		return nil, err
	}
	auditLog, err := OpenAudit(wdb)
	if err != nil {
		return nil, err
	}
	return &Auth{sessions: sessions, auditLog: auditLog}, nil
}

// SetIntrospector attaches the channel-two identity forwarding (the
// cloud-access wiring calls this when the gateway client exists).
func (a *Auth) SetIntrospector(i Introspector) {
	a.mu.Lock()
	a.introspec = i
	a.mu.Unlock()
}

func (a *Auth) introspector() Introspector {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.introspec
}

// Authenticate resolves a request identity. Tunnel assertions come first
// (the edge already validated the caller against the core, §2.2); then
// core-issued session tokens via the mirror with online re-validation:
// fresh results refresh the mirror, revocations kill it with the
// enumerated code, and an unreachable core leaves established sessions
// alive (monotone degradation — new identities still fail closed).
func (a *Auth) Authenticate(ctx context.Context, credential string) (tenancy.Context, error) {
	if credential == "" {
		if actor, ok := ActorFromContext(ctx); ok {
			return actor.Context(), nil
		}
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	if !strings.HasPrefix(credential, sessionScheme+"_") {
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	sess, state, err := a.sessions.lookup(ctx, credential, a.introspector())
	if err != nil {
		return tenancy.Context{}, err
	}
	switch state {
	case sessionActive:
		return sess.Context(), nil
	case sessionRevoked:
		return tenancy.Context{}, &authn.Error{Code: authn.CodeRevoked, Err: authn.ErrUnauthenticated}
	default:
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
}

// SessionCookie carries the bridge login session (HttpOnly, SameSite=Lax).
// EventSource cannot set headers, so cookie sessions also cover SSE (R3).
// The name is this package's protocol detail — the gateway learns it via
// the CookieCredential interface, never as a core constant.
const SessionCookie = "woca_session"

// CookieName implements gateway.CookieCredential.
func (a *Auth) CookieName() string { return SessionCookie }

// RegisterRoutes mounts POST /api/auth/login and POST /api/auth/logout.
// Login is exempt from the gateway credential middleware (it bootstraps
// authentication); logout requires a session.
func (a *Auth) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleLogout)
	a.RegisterAdminRoutes(mux)
}

// handleLogin forwards the credential to the private core over the
// machine channel (§2.7): the core validates the woca_ key and issues
// the opaque session; the bridge mirrors the result and hands the token
// out exactly once (cookie for browsers, bare token for native).
// An unreachable core fails closed — new logins are refused.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// The login exchange takes a user credential (a woca_ key), not a
	// session token.
	if strings.HasPrefix(req.APIKey, sessionScheme+"_") {
		writeAuthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	intro := a.introspector()
	if intro == nil {
		writeAuthError(w, http.StatusServiceUnavailable, "no private cloud connection — login unavailable (fail-closed)")
		return
	}
	sess, err := intro.CoreLogin(r.Context(), req.APIKey)
	if err != nil {
		if errors.Is(err, ErrCoreUnreachable) {
			a.AuditEvent(r.Context(), "unknown", "login_failed", "core unreachable")
			writeAuthErrorCode(w, http.StatusServiceUnavailable, "private core unreachable — login unavailable (fail-closed)", "")
			return
		}
		a.AuditEvent(r.Context(), "unknown", "login_failed", "")
		code := ""
		if errors.Is(err, ErrCoreLoginRevoked) {
			code = authn.CodeRevoked
		}
		writeAuthErrorCode(w, http.StatusUnauthorized, "unauthorized", code)
		return
	}
	// Mirror first so the audit resolution below finds the remark row.
	a.sessions.mirror(sess)
	a.AuditEvent(r.Context(), sess.UserID, "login", sess.Remark)
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(time.Until(time.UnixMilli(sess.ExpiresAt)).Seconds()),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      sess.Token,
		"expires_at": sess.ExpiresAt,
		"tenant_id":  sess.TenantID,
		"user_id":    sess.UserID,
		"remark":     sess.Remark,
		"role":       sess.Role,
	})
}

// handleLogout revokes the calling session locally AND at the core (the
// core is the authority; the local mirror dies either way).
func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := ""
	if c, err := r.Cookie(SessionCookie); err == nil {
		token = c.Value
	}
	if h := r.Header.Get("Authorization"); token == "" && len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		token = strings.TrimSpace(h[7:])
	}
	if token != "" {
		// The core revocation is authoritative — without it the token
		// would re-validate online and resurrect. A dead channel still
		// logs the caller out locally; the online path catches up.
		if intro := a.introspector(); intro != nil {
			_ = intro.CoreLogout(r.Context(), token)
		}
		a.sessions.revoke(token)
		a.AuditEvent(r.Context(), "self", "logout", "")
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// RemarkFor resolves the remark of a mirrored session row (audit and
// "me" surfaces — the identity came from the core; nothing to verify).
func (a *Auth) RemarkFor(userID string) string { return a.sessions.remarkFor(userID) }

// CheckBind enforces the fail-closed red line R1: a non-loopback bind with
// local trust refuses to start — an unauthenticated bridge on the network
// is a public shell.
func CheckBind(addr, authMode string) error {
	if authMode == "" || authMode == "local" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if !isLoopback(host) {
			return fmt.Errorf("bridge: refusing to bind %s with local trust (no authentication): an unauthenticated bridge on the network exposes a shell — use --auth apikey or bind loopback (product design R1)", addr)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- session mirror -------------------------------------------------------

// mirrorSession is the bridge-side record of a core-issued session: a
// validated RESULT (never key material), kept for request routing while
// the core stays the authority. In-memory by design (§2.7: ≤60s
// freshness, non-authoritative, 不落盘) — a bridge restart forgets
// nothing the core does not remember, and re-validates online.
type mirrorSession struct {
	TokenHash string
	TenantID  string
	UserID    string
	Remark    string
	Role      string
	CreatedAt int64
	LastSeen  int64
	ExpiresAt int64
	LastCheck int64
}

// Context renders the mirrored identity. The business tenant (the
// console's unit of accounting) maps onto the bridge's single trust
// circle: on the bridge a tenant is a stats dimension, never an
// isolation boundary — the stored TenantID stays for audit rows.
func (m mirrorSession) Context() tenancy.Context {
	return tenancy.Context{
		TenantID: BridgeTrustTenant,
		UserID:   m.UserID,
		Scopes:   authz.ScopesForRole(m.Role),
	}
}

// BridgeTrustTenant is the tenancy every console-issued credential runs
// under on this bridge (the whole private cloud is one trust circle;
// business units are accounted at the core).
const BridgeTrustTenant = "local"

type sessionState int

const (
	sessionActive sessionState = iota
	sessionRevoked
	sessionInvalid
)

// sessionStore is the in-memory validation-result cache. Entries live
// until the session's natural expiry (the degradation bound) and are
// re-validated against the core at most every recheckGap.
type sessionStore struct {
	mu sync.RWMutex
	m  map[string]mirrorSession // token hash → last validation
}

func newSessionStore(_ *wocadb.DB) (*sessionStore, error) {
	return &sessionStore{m: map[string]mirrorSession{}}, nil
}

// mirror records a core-issued session at login time (the fresh
// validation result — no introspection needed until the gap elapses).
func (s *sessionStore) mirror(sess CoreSession) {
	now := time.Now().UnixMilli()
	m := mirrorSession{
		TokenHash: hashToken(sess.Token), TenantID: sess.TenantID, UserID: sess.UserID,
		Remark: sess.Remark, Role: sess.Role, CreatedAt: now, LastSeen: now,
		ExpiresAt: sess.ExpiresAt, LastCheck: now,
	}
	s.mu.Lock()
	s.m[m.TokenHash] = m
	s.sweepLocked(now)
	s.mu.Unlock()
}

// sweepLocked drops expired entries (caller holds the lock).
func (s *sessionStore) sweepLocked(now int64) {
	for k, v := range s.m {
		if now > v.ExpiresAt {
			delete(s.m, k)
		}
	}
}

func (s *sessionStore) get(hash string) (mirrorSession, bool) {
	s.mu.RLock()
	m, ok := s.m[hash]
	s.mu.RUnlock()
	return m, ok
}

// lookup resolves a session token: the mirror answers while fresh; a
// stale entry re-validates against the core (active → refresh, revoked →
// delete + revoked, invalid → delete); an unreachable core keeps the
// established entry (monotone degradation). A token with no entry is
// validated online once — and fails closed when the core is unreachable.
func (s *sessionStore) lookup(ctx context.Context, token string, intro Introspector) (mirrorSession, sessionState, error) {
	hash := hashToken(token)
	if m, ok := s.get(hash); ok {
		now := time.Now()
		if now.UnixMilli() > m.ExpiresAt {
			s.mu.Lock()
			delete(s.m, hash)
			s.mu.Unlock()
			return mirrorSession{}, sessionInvalid, nil
		}
		if time.Since(time.UnixMilli(m.LastCheck)) <= recheckGap || intro == nil {
			return m, sessionActive, nil
		}
	}
	if intro == nil {
		return mirrorSession{}, sessionInvalid, nil
	}
	return s.validateOnline(ctx, token, intro)
}

// validateOnline asks the core about a token and reconciles the mirror.
func (s *sessionStore) validateOnline(ctx context.Context, token string, intro Introspector) (mirrorSession, sessionState, error) {
	sess, err := intro.CoreIntrospect(ctx, token)
	if err != nil {
		if errors.Is(err, ErrCoreUnreachable) {
			// Degradation: an established entry stands; an unknown
			// token fails closed (nothing local to stand on).
			if m, ok := s.get(hashToken(token)); ok && time.Now().UnixMilli() <= m.ExpiresAt {
				return m, sessionActive, nil
			}
			return mirrorSession{}, sessionInvalid, nil
		}
		return mirrorSession{}, sessionInvalid, err
	}
	hash := hashToken(token)
	if sess.Revoked {
		s.mu.Lock()
		delete(s.m, hash)
		s.mu.Unlock()
		return mirrorSession{}, sessionRevoked, nil
	}
	now := time.Now().UnixMilli()
	m := mirrorSession{
		TokenHash: hash, TenantID: sess.TenantID, UserID: sess.UserID,
		Remark: sess.Remark, Role: sess.Role, CreatedAt: now, LastSeen: now,
		ExpiresAt: sess.ExpiresAt, LastCheck: now,
	}
	s.mu.Lock()
	s.m[hash] = m
	s.sweepLocked(now)
	s.mu.Unlock()
	return m, sessionActive, nil
}

func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.m, hashToken(token))
	s.mu.Unlock()
}

// remarkFor resolves the remembered remark for a mirrored user id
// (audit trail / "me" surfaces name people).
func (s *sessionStore) remarkFor(userID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best mirrorSession
	for _, m := range s.m {
		if m.UserID == userID && m.LastSeen > best.LastSeen {
			best = m
		}
	}
	return best.Remark
}

// list returns the live mirror inventory (the "已登录设备" view).
func (s *sessionStore) list() []mirrorSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]mirrorSession, 0, len(s.m))
	now := time.Now().UnixMilli()
	for _, m := range s.m {
		if now <= m.ExpiresAt {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeJSONAuth(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

func writeAuthError(w http.ResponseWriter, code int, msg string) {
	writeAuthErrorCode(w, code, msg, "")
}

// writeAuthErrorCode emits the standard S-B envelope; the authn reason
// slug survives in the transitional reason field (api-standard.md §1).
func writeAuthErrorCode(w http.ResponseWriter, code int, msg, errCode string) {
	seg, num := httpx.SegBusiness, httpx.CodeUnauthorized
	switch errCode {
	case authn.CodeRevoked:
		num = httpx.CodeCredentialRevoked
	case authn.CodeForbidden:
		num = httpx.CodeForbidden
	}
	body := map[string]string{"error": msg, "code": seg + "-" + num}
	if errCode != "" {
		body["reason"] = errCode
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
