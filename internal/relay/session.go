package relay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Session expiry (design §4.4 — OWASP: sliding expiry needs an absolute
// cap, otherwise a stolen credential renews forever).
const (
	SessionIdleTTL = 7 * 24 * time.Hour
	SessionMaxAge  = 30 * 24 * time.Hour
)

// ErrSessionUnknown rejects absent, expired, or deleted sessions.
var ErrSessionUnknown = errors.New("relay: unknown or expired session")

// Session is a resolved client session (web cookie and native Bearer
// share the sessions table, design §3.3).
type Session struct {
	KeyID     int64
	KeyName   string
	UserID    string
	CreatedAt time.Time
	TouchedAt time.Time
}

// CreateSession mints an opaque 128-bit session token (design §4.4); the
// store keeps only its SHA-256. expiresAt is the absolute cap.
func (s *KeyStore) CreateSession(ctx context.Context, keyID int64) (string, time.Time, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("relay: mint session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, key_id, created_at, touched_at) VALUES (?, ?, ?, ?)`,
		HashToken(token), keyID, now, now); err != nil {
		return "", time.Time{}, fmt.Errorf("relay: create session: %w", err)
	}
	return token, now.Add(SessionMaxAge), nil
}

// ResolveSession validates token and slides its idle expiry forward.
// Errors: ErrSessionUnknown (absent/expired), ErrKeyRevoked (owning key
// revoked while the session row still exists).
func (s *KeyStore) ResolveSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var keyHash []byte
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT s.key_id, s.created_at, s.touched_at, k.name, k.key_hash, k.revoked_at
		FROM sessions s JOIN keys k ON k.id = s.key_id
		WHERE s.token_hash = ?`, HashToken(token)).
		Scan(&sess.KeyID, &sess.CreatedAt, &sess.TouchedAt, &sess.KeyName, &keyHash, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionUnknown
	}
	if err != nil {
		return Session{}, fmt.Errorf("relay: resolve session: %w", err)
	}
	if revokedAt.Valid {
		return Session{}, ErrKeyRevoked
	}
	now := time.Now().UTC()
	if now.Sub(sess.TouchedAt) > SessionIdleTTL || now.Sub(sess.CreatedAt) > SessionMaxAge {
		_ = s.DeleteSession(ctx, token)
		return Session{}, ErrSessionUnknown
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET touched_at = ? WHERE token_hash = ?`, now, HashToken(token)); err != nil {
		return Session{}, fmt.Errorf("relay: touch session: %w", err)
	}
	sess.TouchedAt = now
	sess.UserID = hex.EncodeToString(keyHash)[:8]
	return sess, nil
}

// DeleteSession removes one session (logout is a server-side delete —
// clearing the cookie alone does not count, design §4.4).
func (s *KeyStore) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

// DeleteSessionsForKey removes every session derived from keyID. Kept
// as store API for maintenance paths (e.g. an operator-initiated
// logout-everywhere); the revocation cascade deliberately does NOT call
// it — revocation tombstones sessions so ResolveSession's keys join can
// still answer ErrKeyRevoked (code "revoked") instead of the
// indistinguishable-from-logged-out ErrSessionUnknown (Task 6).
func (s *KeyStore) DeleteSessionsForKey(ctx context.Context, keyID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE key_id = ?`, keyID)
	return err
}
