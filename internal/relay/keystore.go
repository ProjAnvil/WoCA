// Package relay implements the personal-edition remote relay (design
// 2026-09-17-personal-relay-design): the key authority (sqlite), client
// sessions, the parking/generation tunnel plane, and the public HTTP
// surface of the woca-relay binary.
//
// Edition boundary: this package never imports internal/ee; its only
// third-party dependencies are modernc.org/sqlite (the storage driver —
// imported directly, never through internal/wocadb, which blank-imports
// pgx) and zerolog (logging). Both are already in the personal dependency
// baseline (ci/deps-personal.txt).
package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// KeyPrefix marks personal-relay keys (design §2: woca_ is enterprise
// vocabulary; wcr_ keeps support and log forensics unambiguous).
const KeyPrefix = "wcr_"

// GenerationStaleAfter is the generation lease window (design §6.2): a
// generation with no poll for this long is dead and a new generation
// takes over; bridge-online is the same window (25s poll wait + network
// slack). Declared here because the key CLI's online check shares it.
const GenerationStaleAfter = 45 * time.Second

// Sentinels for key lookups.
var (
	ErrKeyUnknown = errors.New("relay: unknown key")
	ErrKeyRevoked = errors.New("relay: key revoked")
)

// Key is one rendezvous-key row. Raw key material is never stored — only
// its SHA-256 (design §4.1).
type Key struct {
	ID         int64
	Name       string
	KeyHash    []byte
	Prefix     string // first 8 chars of the raw key, display only
	CreatedAt  time.Time
	RevokedAt  *time.Time // nil = live
	LastUsedAt *time.Time
}

// UserID is the key's stable public identifier: the first 8 hex chars of
// its stored hash (design §3.3 — the /api/auth/me shape).
func (k Key) UserID() string {
	if len(k.KeyHash) == 0 {
		return ""
	}
	return hex.EncodeToString(k.KeyHash)[:8]
}

// GenerateKey mints a new rendezvous key: 32 bytes of CSPRNG entropy
// base64url-encoded under the wcr_ prefix (256 bits, design §4.1).
func GenerateKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("relay: generate key: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// HashToken is the lookup digest for keys and session tokens: high-entropy
// random tokens hash with plain SHA-256 (design §4.1 — bcrypt/argon2 are
// password constructions and would only add a DoS surface here). Lookup by
// hash also sidesteps timing comparison concerns.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// KeyStore is the relay's only persistent state: rendezvous keys and
// client sessions. The relay holds zero business data (design §8).
type KeyStore struct {
	db *sql.DB
}

const currentSchemaVersion = 1

// Versioned schema_migrations pattern (template: native-client/
// configstore/store.go:36-41 — copied as a pattern only; that package is
// a separate Go module and must not be imported).
const schemaMigrationsDDL = `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
)`

// Schema (design §8).
const keysDDL = `CREATE TABLE keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  key_hash     BLOB NOT NULL UNIQUE,
  prefix       TEXT NOT NULL,
  created_at   TIMESTAMP NOT NULL,
  revoked_at   TIMESTAMP,
  last_used_at TIMESTAMP
)`

const sessionsDDL = `CREATE TABLE sessions (
  token_hash BLOB PRIMARY KEY,
  key_id     INTEGER NOT NULL REFERENCES keys(id),
  created_at TIMESTAMP NOT NULL,
  touched_at TIMESTAMP NOT NULL
)`

// OpenKeyStore opens (creating and migrating as needed) the relay
// database at path.
func OpenKeyStore(path string) (*KeyStore, error) {
	if path == "" {
		return nil, fmt.Errorf("relay: database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("relay: create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("relay: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("relay: ping sqlite: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("relay: configure sqlite: %w", err)
	}
	s := &KeyStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *KeyStore) Close() error { return s.db.Close() }

func (s *KeyStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// schema_migrations itself is absent: fresh database.
		for _, stmt := range []string{schemaMigrationsDDL, keysDDL, sessionsDDL} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("relay: apply schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			currentSchemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("relay: record schema version: %w", err)
		}
		return tx.Commit()
	}
	if int(version.Int64) != currentSchemaVersion {
		return fmt.Errorf("relay: unsupported schema version %d (binary supports %d)",
			version.Int64, currentSchemaVersion)
	}
	return tx.Commit()
}

// CreateKey mints and stores a new key, returning the row and the full
// raw key — the ONLY time the raw key is available (design §4.1).
func (s *KeyStore) CreateKey(ctx context.Context, name string) (Key, string, error) {
	if name == "" || len(name) > 128 {
		return Key{}, "", fmt.Errorf("relay: key name must be 1-128 characters, got %d", len(name))
	}
	raw, err := GenerateKey()
	if err != nil {
		return Key{}, "", err
	}
	prefix := raw
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO keys (name, key_hash, prefix, created_at) VALUES (?, ?, ?, ?)`,
		name, HashToken(raw), prefix, now)
	if err != nil {
		return Key{}, "", fmt.Errorf("relay: create key %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Key{}, "", err
	}
	return Key{ID: id, Name: name, KeyHash: HashToken(raw), Prefix: prefix, CreatedAt: now}, raw, nil
}

const keySelect = `SELECT id, name, key_hash, prefix, created_at, revoked_at, last_used_at FROM keys`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(row rowScanner) (Key, error) {
	var k Key
	var revokedAt, lastUsedAt sql.NullTime
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &k.CreatedAt, &revokedAt, &lastUsedAt); err != nil {
		return Key{}, err
	}
	if revokedAt.Valid {
		k.RevokedAt = &revokedAt.Time
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = &lastUsedAt.Time
	}
	return k, nil
}

// LookupKey resolves a raw key by its SHA-256 (design §4.1). Errors:
// ErrKeyUnknown (absent or malformed), ErrKeyRevoked.
func (s *KeyStore) LookupKey(ctx context.Context, raw string) (Key, error) {
	if len(raw) != len(KeyPrefix)+43 || raw[:len(KeyPrefix)] != KeyPrefix {
		return Key{}, ErrKeyUnknown
	}
	k, err := scanKey(s.db.QueryRowContext(ctx, keySelect+` WHERE key_hash = ?`, HashToken(raw)))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrKeyUnknown
	}
	if err != nil {
		return Key{}, fmt.Errorf("relay: lookup key: %w", err)
	}
	if k.RevokedAt != nil {
		return k, ErrKeyRevoked
	}
	return k, nil
}

// ListKeys returns every key, oldest first (the `key list` CLI).
func (s *KeyStore) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, keySelect+` ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("relay: list keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks name revoked (design §5). ok is false when the key is
// unknown or already revoked — revoking is idempotent at the CLI level.
func (s *KeyStore) RevokeKey(ctx context.Context, name string) (Key, bool, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE keys SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`, now, name)
	if err != nil {
		return Key{}, false, fmt.Errorf("relay: revoke key %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Key{}, false, err
	}
	if n == 0 {
		return Key{}, false, nil
	}
	k, err := scanKey(s.db.QueryRowContext(ctx, keySelect+` WHERE name = ?`, name))
	if err != nil {
		return Key{}, false, fmt.Errorf("relay: reload revoked key: %w", err)
	}
	return k, true, nil
}

// TouchKey stamps last_used_at (bridge tunnel activity — the `key list`
// online check derives from it: last use within GenerationStaleAfter).
func (s *KeyStore) TouchKey(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE keys SET last_used_at = ? WHERE id = ?`, time.Now().UTC(), id)
}
