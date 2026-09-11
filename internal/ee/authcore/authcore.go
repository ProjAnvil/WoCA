// Package authcore is the private core's user-identity authority
// (product design v1.8 §2.1): woca_ keys validate here, opaque sessions
// are issued, introspected, and revoked here. Bridges hold zero user
// credentials — the edge validates client traffic by calling home, and
// direct-connect bridges forward logins over the machine channel
// (introspection, §2.7). Revocation is immediate: the next validation
// fails; nothing about a user identity is ever synchronized outward.
package authcore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// sessionScheme prefixes core-issued session tokens (the shape every
// consumer — browser cookie, native Bearer, introspection cache — uses).
const sessionScheme = "wocs"

// sessionTTL is the session lifetime, sliding on validation (§2.1:
// 7-day sliding, revocable).
const sessionTTL = 7 * 24 * time.Hour

// slideRewriteGap throttles sliding-expiry row writes.
const slideRewriteGap = time.Hour

// Session is one core-issued login session. Token is populated exactly
// once, at issue time; every other surface works from the hash.
type Session struct {
	Token      string `json:"token,omitempty"`
	TokenHash  string `json:"token_hash,omitempty"`
	KeyPrefix  string `json:"key_prefix"`
	TenantName string `json:"tenant_name,omitempty"`
	TenantID   string `json:"tenant_id"`
	UserID     string `json:"user_id"`
	Remark     string `json:"remark"`
	Role       string `json:"role"`
	CreatedAt  int64  `json:"created_at"`
	LastSeen   int64  `json:"last_seen"`
	ExpiresAt  int64  `json:"expires_at"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
}

// Context renders the session's tenancy context.
func (s Session) Context() tenancy.Context {
	return tenancy.Context{
		TenantID: s.TenantID,
		UserID:   s.UserID,
		Scopes:   authz.ScopesForRole(s.Role),
	}
}

// Service is the session authority over the shared database.
type Service struct {
	keys *tenant.Store
	db   *wocadb.DB
	log  zerolog.Logger
}

// New migrates the session table and returns the service.
func New(keys *tenant.Store, db *wocadb.DB, log zerolog.Logger) (*Service, error) {
	// Timestamp columns hold unix milliseconds — BIGINT on postgres
	// (int4 overflows; the convention wocadb's own tables follow).
	ts := "BIGINT"
	if db.Dialect != wocadb.DialectPostgres {
		ts = "INTEGER"
	}
	if err := db.WithMigrateLock(func(q wocadb.Querier) error {
		if _, err := q.Exec(`CREATE TABLE IF NOT EXISTS core_sessions (
			token_hash TEXT PRIMARY KEY,
			key_prefix TEXT NOT NULL,
			tenant_id  TEXT NOT NULL,
			user_id    TEXT NOT NULL,
			remark     TEXT NOT NULL,
			role       TEXT NOT NULL,
			created_at ` + ts + ` NOT NULL,
			last_seen  ` + ts + ` NOT NULL,
			expires_at ` + ts + ` NOT NULL,
			revoked_at ` + ts + `
		)`); err != nil {
			return fmt.Errorf("authcore: migrate: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &Service{keys: keys, db: db, log: log}, nil
}

func (s *Service) ph(i int) string { return s.db.Dialect.Placeholder(i) }

func nowMilli() int64 { return time.Now().UnixMilli() }

// Login validates a woca_ key and issues one opaque session. The full
// token returns exactly once; every later reference is by hash.
func (s *Service) Login(_ context.Context, apiKey string) (Session, error) {
	k, ok, err := s.keys.LookupKey(apiKey)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		if k.RevokedAt != 0 {
			return Session{}, &authn.Error{Code: authn.CodeRevoked, Err: authn.ErrUnauthenticated}
		}
		return Session{}, authn.ErrUnauthenticated
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	token := sessionScheme + "_" + hex.EncodeToString(raw)
	now := nowMilli()
	sess := Session{
		Token:     token,
		KeyPrefix: k.Prefix,
		TenantID:  k.TenantID,
		UserID:    k.UserID,
		Remark:    k.Name,
		Role:      k.Role,
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: now + sessionTTL.Milliseconds(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO core_sessions (token_hash, key_prefix, tenant_id, user_id, remark, role, created_at, last_seen, expires_at)
		 VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`, `+s.ph(5)+`, `+s.ph(6)+`, `+s.ph(7)+`, `+s.ph(8)+`)`,
		hashToken(token), sess.KeyPrefix, sess.TenantID, sess.UserID, sess.Remark, sess.Role, sess.CreatedAt, sess.LastSeen, sess.ExpiresAt); err != nil {
		return Session{}, fmt.Errorf("authcore: issue session: %w", err)
	}
	s.keys.TouchLastUsed(k.Prefix)
	return sess, nil
}

// Introspect is the authoritative validation (RFC 7662-shaped): it
// resolves a session token to its identity, slides the expiry, and
// classifies failures — revoked carries the enumerated code the clients
// switch on, everything else is a uniform unauthenticated.
func (s *Service) Introspect(_ context.Context, token string) (Session, error) {
	var sess Session
	var revoked sql.NullInt64
	err := s.db.QueryRow(
		`SELECT key_prefix, tenant_id, user_id, remark, role, created_at, last_seen, expires_at, revoked_at
		 FROM core_sessions WHERE token_hash = `+s.ph(0), hashToken(token)).
		Scan(&sess.KeyPrefix, &sess.TenantID, &sess.UserID, &sess.Remark, &sess.Role,
			&sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, authn.ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("authcore: introspect: %w", err)
	}
	if revoked.Valid {
		return Session{}, &authn.Error{Code: authn.CodeRevoked, Err: authn.ErrUnauthenticated}
	}
	now := nowMilli()
	if now > sess.ExpiresAt {
		// Naturally expired: drop the row, report unauthenticated. A
		// failed cleanup leaves a dead row the next sweep retires; it
		// must be visible, not silent.
		if _, err := s.db.Exec(`DELETE FROM core_sessions WHERE token_hash = `+s.ph(0), hashToken(token)); err != nil {
			s.log.Warn().Err(err).Msg("authcore: expired-session cleanup failed")
		}
		return Session{}, authn.ErrUnauthenticated
	}
	if time.Since(time.UnixMilli(sess.LastSeen)) > slideRewriteGap {
		// A failed slide silently shortens the session (it re-slides on
		// the next introspect past the gap) — log so the drift is
		// observable.
		if _, err := s.db.Exec(
			`UPDATE core_sessions SET last_seen = `+s.ph(0)+`, expires_at = `+s.ph(1)+` WHERE token_hash = `+s.ph(2),
			now, now+sessionTTL.Milliseconds(), hashToken(token)); err != nil {
			s.log.Warn().Err(err).Msg("authcore: session slide failed")
		}
		s.keys.TouchLastUsed(sess.KeyPrefix)
	}
	return sess, nil
}

// Logout revokes the caller's own session (the row stays, marked — the
// console's session audit reads it).
func (s *Service) Logout(_ context.Context, token string) error {
	return s.revokeByHash(hashToken(token))
}

// RevokeSession revokes one session by token hash (the console action).
func (s *Service) RevokeSession(tokenHash string) error {
	return s.revokeByHash(tokenHash)
}

func (s *Service) revokeByHash(hash string) error {
	res, err := s.db.Exec(`UPDATE core_sessions SET revoked_at = `+s.ph(0)+` WHERE token_hash = `+s.ph(1)+` AND revoked_at IS NULL`,
		nowMilli(), hash)
	if err != nil {
		return fmt.Errorf("authcore: revoke session: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("unknown session")
	}
	return nil
}

// RevokeKeySessions marks every live session of a key revoked — the
// immediate cascade when the console revokes the key (§2.1: revocation
// is final; the next validation fails).
func (s *Service) RevokeKeySessions(_ context.Context, keyPrefix string) error {
	_, err := s.db.Exec(`UPDATE core_sessions SET revoked_at = `+s.ph(0)+` WHERE key_prefix = `+s.ph(1)+` AND revoked_at IS NULL`,
		nowMilli(), keyPrefix)
	if err != nil {
		return fmt.Errorf("authcore: revoke key sessions: %w", err)
	}
	return nil
}

// RevokeTenantSessions marks every live session of a business unit
// revoked — the cascade when the console deletes the tenant. The rows
// stay (the session audit reads them), pointing at a tenant that no
// longer exists.
func (s *Service) RevokeTenantSessions(_ context.Context, tenantID string) error {
	_, err := s.db.Exec(`UPDATE core_sessions SET revoked_at = `+s.ph(0)+` WHERE tenant_id = `+s.ph(1)+` AND revoked_at IS NULL`,
		nowMilli(), tenantID)
	if err != nil {
		return fmt.Errorf("authcore: revoke tenant sessions: %w", err)
	}
	return nil
}

// ListSessions returns one page of the session inventory (revoked rows
// included, marked) for the console: q filters by remark substring
// (case-insensitive); limit/offset page through the database. total
// counts the filtered set for the pager.
func (s *Service) ListSessions(_ context.Context, q, order string, limit, offset int) ([]Session, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	like := "%" + strings.ToLower(q) + "%"
	// q matches the remark (username), the backing key prefix, or the
	// tenant name (business unit) — all fuzzy.
	cond := ""
	var args []any
	if q != "" {
		cond = ` WHERE (LOWER(c.remark) LIKE ` + s.ph(0) + ` OR LOWER(c.key_prefix) LIKE ` + s.ph(1) + ` OR LOWER(t.name) LIKE ` + s.ph(2) + `)`
		args = append(args, like, like, like)
	}
	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM core_sessions c LEFT JOIN tenants t ON t.id = c.tenant_id`+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// Sorting is whitelist-only; the ORDER BY never interpolates raw input.
	orderSQL := "c.last_seen DESC"
	switch {
	case order == "created_at":
		orderSQL = "c.created_at"
	case order == "created_at_desc":
		orderSQL = "c.created_at DESC"
	case order == "expires_at":
		orderSQL = "c.expires_at"
	case order == "expires_at_desc":
		orderSQL = "c.expires_at DESC"
	case order == "last_seen":
		orderSQL = "c.last_seen"
	}
	rows, err := s.db.Query(
		`SELECT c.token_hash, c.key_prefix, c.tenant_id, c.user_id, c.remark, c.role, c.created_at, c.last_seen, c.expires_at, COALESCE(c.revoked_at, 0), COALESCE(t.name, '')
		 FROM core_sessions c LEFT JOIN tenants t ON t.id = c.tenant_id`+cond+`
		 ORDER BY `+orderSQL+` LIMIT `+strconv.Itoa(limit)+` OFFSET `+strconv.Itoa(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []Session{}
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.TokenHash, &sess.KeyPrefix, &sess.TenantID, &sess.UserID, &sess.Remark, &sess.Role,
			&sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt, &sess.RevokedAt, &sess.TenantName); err != nil {
			return nil, 0, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Handler serves the machine-channel auth surface. Core-internal (the
// private network is the boundary, like /gw/resolve): the edge calls it
// home as the auth enforcement point, and direct-connect bridges reach
// it through the edge's /gw forwarding with their machine tokens.
func Handler(s *Service, audit func(event, subject, detail string)) http.Handler {
	mux := http.NewServeMux()
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(dst); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return false
		}
		return true
	}
	mux.HandleFunc("POST /gw/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIKey string `json:"api_key"`
		}
		if !decode(w, r, &req) {
			return
		}
		sess, err := s.Login(r.Context(), req.APIKey)
		if err != nil {
			if audit != nil {
				audit("login_failed", "", "")
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "code": authn.CodeOf(err)})
			return
		}
		if audit != nil {
			audit("login", sess.UserID, "remark="+sess.Remark+" prefix="+sess.KeyPrefix)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      sess.Token,
			"expires_at": sess.ExpiresAt,
			"tenant_id":  sess.TenantID,
			"user_id":    sess.UserID,
			"remark":     sess.Remark,
			"role":       sess.Role,
		})
	})
	mux.HandleFunc("POST /gw/auth/introspect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		sess, err := s.Introspect(r.Context(), req.Token)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"active": false, "code": authn.CodeOf(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"active":     true,
			"key_prefix": sess.KeyPrefix,
			"tenant_id":  sess.TenantID,
			"user_id":    sess.UserID,
			"remark":     sess.Remark,
			"role":       sess.Role,
			"expires_at": sess.ExpiresAt,
		})
	})
	mux.HandleFunc("POST /gw/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := s.Logout(r.Context(), req.Token); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if audit != nil {
			audit("session_revoked", "", "logout")
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}
