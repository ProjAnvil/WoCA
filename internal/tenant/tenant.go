// Package tenant implements the tenant model of the platform edition:
// tenants, users, and API keys over the woca database (sqlite or postgres).
//
// API keys are the Phase 1 credential: high-entropy random strings in the
// form woca_<prefix>_<secret>. Only a SHA-256 hash of the full key is
// stored; the prefix is non-secret and doubles as the lookup handle. RLS
// does not apply to these tables — the authentication lookup must resolve
// the tenant from the credential itself, before any tenant context exists
// (the bootstrap-table exception; all business tables are RLS-scoped).
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/wocadb"
)

// KeyPrefixLen and keySecretLen are in hex characters.
const (
	keyPrefixLen = 12
	keySecretLen = 48
	keyScheme    = "woca"
)

// Tenant is one isolation boundary on the platform.
type Tenant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// User is a member of a tenant.
type User struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// APIKey is one issued credential (the secret is shown once at issue time).
type APIKey struct {
	Prefix    string `json:"prefix"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
	RevokedAt int64  `json:"revoked_at,omitempty"`
}

// ErrNotFound is returned when a tenant, user, or key does not exist.
var ErrNotFound = errors.New("tenant: not found")

// Store manages tenants, users, and API keys on a woca database. It is safe
// for concurrent use (all state lives in the database).
type Store struct {
	db      *sql.DB
	dialect wocadb.Dialect
	// wdb is the wocadb wrapper the store was opened over (nil when built
	// directly over a *sql.DB); it lets callers layer other stores (e.g.,
	// settings for the notification plane) on the same pool.
	wdb *wocadb.DB
}

// NewStore migrates the tenant tables on db and returns a Store.
func NewStore(db *wocadb.DB) (*Store, error) {
	s := &Store{db: db.DB, dialect: db.Dialect, wdb: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// migrate creates the tenant schema idempotently. These are the identity
// bootstrap tables; business tables carry tenant_id + RLS instead (see
// wocadb).
func (s *Store) migrate() error {
	ts := "BIGINT"
	if s.dialect != wocadb.DialectPostgres {
		ts = "INTEGER"
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at ` + ts + ` NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL REFERENCES tenants(id),
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			created_at ` + ts + ` NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			prefix TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL REFERENCES tenants(id),
			user_id TEXT NOT NULL REFERENCES users(id),
			key_hash TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			created_at ` + ts + ` NOT NULL,
			revoked_at ` + ts + `
		)`,
	}
	err := wocadb.WithMigrateLock(s.dialect, s.db, func(qw wocadb.Querier) error {
		for _, q := range stmts {
			if _, err := qw.Exec(q); err != nil {
				return fmt.Errorf("tenant: migrate: %w", err)
			}
		}
		return s.migrateNativeSessions(ts, qw)
	})
	if err != nil {
		return err
	}
	// Bootstrap the machine tenant and its default user so credentials can
	// be issued for the local machine identity (e.g. a read-only key for a
	// LAN-exposed personal server).
	now := fmt.Sprintf("%d", nowMilli())
	seed := []string{
		`INSERT INTO tenants (id, name, created_at) VALUES ('local', 'local machine', ` + now + `)
		 ON CONFLICT(id) DO NOTHING`,
		`INSERT INTO users (id, tenant_id, name, role, created_at) VALUES ('u_local', 'local', 'machine', '` + authz.RoleMember + `', ` + now + `)
		 ON CONFLICT(id) DO NOTHING`,
	}
	for _, q := range seed {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("tenant: migrate: seed: %w", err)
		}
	}
	return nil
}

// ph returns the i-th (0-based) placeholder for the dialect.
func (s *Store) ph(i int) string {
	if s.dialect == wocadb.DialectPostgres {
		return fmt.Sprintf("$%d", i+1)
	}
	return "?"
}

// CreateTenant registers a tenant with a random id ("t_<hex8>").
func (s *Store) CreateTenant(_ context.Context, name string) (Tenant, error) {
	id, err := randomHex(4)
	if err != nil {
		return Tenant{}, err
	}
	t := Tenant{ID: "t_" + id, Name: name, CreatedAt: nowMilli()}
	_, err = s.db.Exec(
		`INSERT INTO tenants (id, name, created_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`)`,
		t.ID, t.Name, t.CreatedAt)
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: create: %w", err)
	}
	return t, nil
}

// ListTenants returns all tenants ordered by id.
func (s *Store) ListTenants(_ context.Context) ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateUser adds a member to a tenant. Role must be one of the authz role
// names; the role only takes effect through keys issued for the user.
func (s *Store) CreateUser(_ context.Context, tenantID, name, role string) (User, error) {
	if len(authz.ScopesForRole(role)) == 0 && role != "" {
		return User{}, fmt.Errorf("tenant: unknown role %q", role)
	}
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tenants WHERE id = `+s.ph(0)+`)`, tenantID).Scan(&exists)
	if err != nil {
		return User{}, fmt.Errorf("tenant: create user: %w", err)
	}
	if !exists {
		return User{}, fmt.Errorf("tenant: create user: %w: tenant %s", ErrNotFound, tenantID)
	}
	id, err := randomHex(4)
	if err != nil {
		return User{}, err
	}
	u := User{ID: "u_" + id, TenantID: tenantID, Name: name, Role: role, CreatedAt: nowMilli()}
	_, err = s.db.Exec(
		`INSERT INTO users (id, tenant_id, name, role, created_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`)`,
		u.ID, u.TenantID, u.Name, u.Role, u.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("tenant: create user: %w", err)
	}
	return u, nil
}

// IssueKey generates and stores a new API key for a user. The full key
// material is returned exactly once; only its SHA-256 hash is persisted.
// The key's role is frozen at issue time (the credential is the role, not
// the mutable user row).
func (s *Store) IssueKey(_ context.Context, tenantID, userID, name, role string) (APIKey, string, error) {
	if len(authz.ScopesForRole(role)) == 0 {
		return APIKey{}, "", fmt.Errorf("tenant: unknown role %q", role)
	}
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = `+s.ph(0)+` AND tenant_id = `+s.ph(1)+`)`,
		userID, tenantID).Scan(&exists)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("tenant: issue key: %w", err)
	}
	if !exists {
		return APIKey{}, "", fmt.Errorf("tenant: issue key: %w: user %s in tenant %s", ErrNotFound, userID, tenantID)
	}

	// Retry on the (astronomically unlikely) prefix collision.
	for attempt := 0; attempt < 5; attempt++ {
		prefix, err := randomHex(keyPrefixLen / 2)
		if err != nil {
			return APIKey{}, "", err
		}
		secret, err := randomHex(keySecretLen / 2)
		if err != nil {
			return APIKey{}, "", err
		}
		full := keyScheme + "_" + prefix + "_" + secret
		k := APIKey{
			Prefix:    prefix,
			TenantID:  tenantID,
			UserID:    userID,
			Name:      name,
			Role:      role,
			CreatedAt: nowMilli(),
		}
		_, err = s.db.Exec(
			`INSERT INTO api_keys (prefix, tenant_id, user_id, key_hash, name, role, created_at, revoked_at)
			 VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`, `+s.ph(5)+`, `+s.ph(6)+`, NULL)`,
			k.Prefix, k.TenantID, k.UserID, hashKey(full), k.Name, k.Role, k.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) && attempt < 4 {
				continue
			}
			return APIKey{}, "", fmt.Errorf("tenant: issue key: %w", err)
		}
		return k, full, nil
	}
	return APIKey{}, "", errors.New("tenant: issue key: prefix collision")
}

// ListKeys returns a tenant's keys (hashes are not returned).
func (s *Store) ListKeys(_ context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.db.Query(
		`SELECT prefix, tenant_id, user_id, name, role, created_at, revoked_at
		 FROM api_keys WHERE tenant_id = `+s.ph(0)+` ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenant: list keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var revoked sql.NullInt64
		if err := rows.Scan(&k.Prefix, &k.TenantID, &k.UserID, &k.Name, &k.Role, &k.CreatedAt, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid {
			k.RevokedAt = revoked.Int64
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks a key inactive. Revoking an unknown key is not an error.
func (s *Store) RevokeKey(_ context.Context, prefix string) error {
	_, err := s.db.Exec(
		`UPDATE api_keys SET revoked_at = `+s.ph(0)+` WHERE prefix = `+s.ph(1)+` AND revoked_at IS NULL`,
		nowMilli(), prefix)
	if err != nil {
		return fmt.Errorf("tenant: revoke key: %w", err)
	}
	return nil
}

// lookup resolves a full key to its row and verifies the hash in constant
// time. It returns the row's tenant/user/role for building the identity.
func (s *Store) lookup(full string) (APIKey, bool, error) {
	// Parse before touching the database: anything not matching the issued
	// format is a reject, not a lookup.
	parts := strings.Split(full, "_")
	if len(parts) != 3 || parts[0] != keyScheme || len(parts[1]) != keyPrefixLen || len(parts[2]) != keySecretLen {
		return APIKey{}, false, nil
	}
	if !isHex(parts[1]) || !isHex(parts[2]) {
		return APIKey{}, false, nil
	}
	var (
		k       APIKey
		keyHash string
		revoked sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT prefix, tenant_id, user_id, name, role, created_at, revoked_at, key_hash
		 FROM api_keys WHERE prefix = `+s.ph(0), parts[1]).
		Scan(&k.Prefix, &k.TenantID, &k.UserID, &k.Name, &k.Role, &k.CreatedAt, &revoked, &keyHash)
	if errors.Is(err, sql.ErrNoRows) {
		// Compare anyway so unknown-prefix and wrong-secret responses are
		// indistinguishable in timing.
		hashKey(full)
		return APIKey{}, false, nil
	}
	if err != nil {
		return APIKey{}, false, fmt.Errorf("tenant: lookup: %w", err)
	}
	if revoked.Valid {
		return APIKey{}, false, nil
	}
	if !constantTimeEqualHex(hashKey(full), keyHash) {
		return APIKey{}, false, nil
	}
	return k, true, nil
}

func randomHex(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tenant: entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqualHex compares two hex digests in constant time.
func constantTimeEqualHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// isUniqueViolation reports a primary/unique constraint failure across the
// supported drivers (sqlite message match + pg error code 23505).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "23505")
}

func nowMilli() int64 { return time.Now().UnixMilli() }

// DB returns the wocadb wrapper the store was opened over (nil when the
// store was built over a bare *sql.DB).
func (s *Store) DB() *wocadb.DB { return s.wdb }

// Generation returns a cheap change fingerprint of the tenant plane: the
// summed creation timestamps of every tenant, user, and API key plus the
// revoked-key count. The tables carry no updated_at, but every mutation
// either inserts a row or flips a key's revoked_at, so this sum is strictly
// monotonic across mutations — exactly what a scheduler's or server's
// generation watcher needs (design §16.7).
func (s *Store) Generation() string {
	if s == nil || s.db == nil {
		return ""
	}
	var gen, revoked string
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(created_at), 0) FROM tenants`).Scan(&gen)
	var users, keys string
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(created_at), 0) FROM users`).Scan(&users)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(created_at), 0) FROM api_keys`).Scan(&keys)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NOT NULL`).Scan(&revoked)
	return gen + "/" + users + "/" + keys + "/" + revoked
}
