// session.go implements the native-client opaque sessions (M2 credential).
//
// A native session is issued once by the activation endpoint in exchange for
// an API key plus a device-generated DPoP public key. The session id has the
// same shape and entropy as an API key (wsess_<prefix>_<secret>): only a
// SHA-256 hash of the full id is stored, the prefix is the non-secret lookup
// handle, and the row pins the DPoP JWK thumbprint that every session-authed
// request must prove possession of (RFC 9449). Like api_keys, these are
// identity bootstrap tables resolved before any tenant context exists, so RLS
// does not apply here (the bootstrap-table exception).
package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

const (
	sessionPrefixLen = 12
	sessionSecretLen = 48
	sessionScheme    = "wsess"
)

// NativeSession is one issued native-client session. The full id (the DPoP
// access token) is returned exactly once at issue time.
type NativeSession struct {
	Prefix          string `json:"prefix"`
	TenantID        string `json:"tenant_id"`
	UserID          string `json:"user_id"`
	DeviceID        string `json:"device_id"`
	Role            string `json:"role"`
	DPoPThumbprint  string `json:"dpop_thumbprint"`
	ExpiresAt       int64  `json:"expires_at"`
	CreatedAt       int64  `json:"created_at"`
	LastValidatedAt int64  `json:"last_validated_at"`
	RevokedAt       int64  `json:"revoked_at,omitempty"`
}

// migrateNativeSessions creates the session table idempotently. Called by
// Store.migrate under the shared migration lock; q is the locked querier.
func (s *Store) migrateNativeSessions(ts string, q wocadb.Querier) error {
	stmt := `CREATE TABLE IF NOT EXISTS native_sessions (
		prefix TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL REFERENCES tenants(id),
		user_id TEXT NOT NULL REFERENCES users(id),
		id_hash TEXT NOT NULL,
		device_id TEXT NOT NULL,
		role TEXT NOT NULL,
		dpop_thumbprint TEXT NOT NULL,
		expires_at ` + ts + ` NOT NULL,
		created_at ` + ts + ` NOT NULL,
		last_validated_at ` + ts + ` NOT NULL DEFAULT 0,
		revoked_at ` + ts + `
	)`
	if _, err := q.Exec(stmt); err != nil {
		return fmt.Errorf("tenant: migrate native_sessions: %w", err)
	}
	return nil
}

// IssueSession generates and stores a native session for a user, pinning the
// device id and DPoP JWK thumbprint. The full session id (the DPoP access
// token) is returned exactly once; only its SHA-256 hash is persisted.
func (s *Store) IssueSession(_ context.Context, tenantID, userID, deviceID, thumbprint, role string, ttl time.Duration) (NativeSession, string, error) {
	if deviceID == "" || thumbprint == "" {
		return NativeSession{}, "", errors.New("tenant: issue session: device id and DPoP thumbprint are required")
	}
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = `+s.ph(0)+` AND tenant_id = `+s.ph(1)+`)`,
		userID, tenantID).Scan(&exists)
	if err != nil {
		return NativeSession{}, "", fmt.Errorf("tenant: issue session: %w", err)
	}
	if !exists {
		return NativeSession{}, "", fmt.Errorf("tenant: issue session: %w: user %s in tenant %s", ErrNotFound, userID, tenantID)
	}

	now := nowMilli()
	for attempt := 0; attempt < 5; attempt++ {
		prefix, err := randomHex(sessionPrefixLen / 2)
		if err != nil {
			return NativeSession{}, "", err
		}
		secret, err := randomHex(sessionSecretLen / 2)
		if err != nil {
			return NativeSession{}, "", err
		}
		full := sessionScheme + "_" + prefix + "_" + secret
		sess := NativeSession{
			Prefix:         prefix,
			TenantID:       tenantID,
			UserID:         userID,
			DeviceID:       deviceID,
			Role:           role,
			DPoPThumbprint: thumbprint,
			ExpiresAt:      now + ttl.Milliseconds(),
			CreatedAt:      now,
		}
		_, err = s.db.Exec(
			`INSERT INTO native_sessions (prefix, tenant_id, user_id, id_hash, device_id, role, dpop_thumbprint, expires_at, created_at, last_validated_at, revoked_at)
			 VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`, `+s.ph(5)+`, `+s.ph(6)+`, `+s.ph(7)+`, `+s.ph(8)+`, 0, NULL)`,
			sess.Prefix, sess.TenantID, sess.UserID, hashKey(full), sess.DeviceID, sess.Role, sess.DPoPThumbprint, sess.ExpiresAt, sess.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) && attempt < 4 {
				continue
			}
			return NativeSession{}, "", fmt.Errorf("tenant: issue session: %w", err)
		}
		return sess, full, nil
	}
	return NativeSession{}, "", errors.New("tenant: issue session: prefix collision")
}

// LookupSession resolves a full session id and verifies the hash in constant
// time. Expired or revoked sessions do not authenticate. Errors from the
// database are returned; a merely invalid session yields ok=false.
func (s *Store) LookupSession(_ context.Context, full string) (NativeSession, bool, error) {
	parts := strings.Split(full, "_")
	if len(parts) != 3 || parts[0] != sessionScheme ||
		len(parts[1]) != sessionPrefixLen || len(parts[2]) != sessionSecretLen {
		return NativeSession{}, false, nil
	}
	if !isHex(parts[1]) || !isHex(parts[2]) {
		return NativeSession{}, false, nil
	}
	var (
		sess    NativeSession
		idHash  string
		revoked sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT prefix, tenant_id, user_id, device_id, role, dpop_thumbprint, expires_at, created_at, last_validated_at, revoked_at, id_hash
		 FROM native_sessions WHERE prefix = `+s.ph(0), parts[1]).
		Scan(&sess.Prefix, &sess.TenantID, &sess.UserID, &sess.DeviceID, &sess.Role, &sess.DPoPThumbprint,
			&sess.ExpiresAt, &sess.CreatedAt, &sess.LastValidatedAt, &revoked, &idHash)
	if errors.Is(err, sql.ErrNoRows) {
		hashKey(full)
		return NativeSession{}, false, nil
	}
	if err != nil {
		return NativeSession{}, false, fmt.Errorf("tenant: lookup session: %w", err)
	}
	if revoked.Valid || sess.ExpiresAt <= nowMilli() {
		return NativeSession{}, false, nil
	}
	if !constantTimeEqualHex(hashKey(full), idHash) {
		return NativeSession{}, false, nil
	}
	return sess, true, nil
}

// RevokeSession marks a session inactive. Revoking an unknown session is not
// an error. The full id is accepted (the caller holds it as its credential).
func (s *Store) RevokeSession(_ context.Context, full string) error {
	prefix, ok := sessionPrefix(full)
	if !ok {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE native_sessions SET revoked_at = `+s.ph(0)+` WHERE prefix = `+s.ph(1)+` AND revoked_at IS NULL`,
		nowMilli(), prefix)
	if err != nil {
		return fmt.Errorf("tenant: revoke session: %w", err)
	}
	return nil
}

// TouchSession records the last successful validation timestamp. The value is
// caller-supplied so tests are deterministic.
func (s *Store) TouchSession(_ context.Context, full string, atMilli int64) error {
	prefix, ok := sessionPrefix(full)
	if !ok {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE native_sessions SET last_validated_at = `+s.ph(0)+` WHERE prefix = `+s.ph(1),
		atMilli, prefix)
	if err != nil {
		return fmt.Errorf("tenant: touch session: %w", err)
	}
	return nil
}

func sessionPrefix(full string) (string, bool) {
	parts := strings.Split(full, "_")
	if len(parts) != 3 || parts[0] != sessionScheme || len(parts[1]) != sessionPrefixLen || !isHex(parts[1]) {
		return "", false
	}
	return parts[1], true
}
