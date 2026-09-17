package configstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/net/idna"
	_ "modernc.org/sqlite"
)

type Profile struct {
	ID        int64  `json:"id"`
	BaseURL   string `json:"base_url"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Store struct {
	db                *sql.DB
	preferenceWriteMu sync.Mutex
	epoch             int64
}

const currentSchemaVersion = 2

const schemaMigrationsDDL = `CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

const connectionProfilesDDL = `CREATE TABLE connection_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    base_url TEXT NOT NULL UNIQUE,
    active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
)`

const oneActiveProfileDDL = `CREATE UNIQUE INDEX one_active_profile
    ON connection_profiles(active) WHERE active = 1`

const preferencesDDL = `CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
)`

const nativeSessionsDDL = `CREATE TABLE native_sessions (
    profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    session_ciphertext BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    last_validated_at INTEGER NOT NULL
)`

const preferenceEpochDDL = `CREATE TABLE preference_epoch (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    epoch INTEGER NOT NULL CHECK (epoch >= 0)
)`

const preferenceVersionsDDL = `CREATE TABLE preference_versions (
    key TEXT PRIMARY KEY REFERENCES preferences(key) ON DELETE CASCADE,
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    revision INTEGER NOT NULL CHECK (revision > 0)
)`

func OpenDefault() (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(path)
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure SQLite database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) SaveProfile(ctx context.Context, rawURL string) (Profile, error) {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return Profile{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO connection_profiles (base_url, active, created_at, updated_at)
		VALUES (?, 0, ?, ?)
		ON CONFLICT(base_url) DO UPDATE SET updated_at = excluded.updated_at`, baseURL, now, now); err != nil {
		return Profile{}, fmt.Errorf("upsert connection profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connection_profiles SET active = 0 WHERE active = 1`); err != nil {
		return Profile{}, fmt.Errorf("clear active connection profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connection_profiles SET active = 1, updated_at = ? WHERE base_url = ?`, now, baseURL); err != nil {
		return Profile{}, fmt.Errorf("activate connection profile: %w", err)
	}

	profile, err := scanProfile(tx.QueryRowContext(ctx, `
		SELECT id, base_url, created_at, updated_at
		FROM connection_profiles WHERE base_url = ?`, baseURL))
	if err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (s *Store) ActiveProfile(ctx context.Context) (Profile, bool, error) {
	profile, err := scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, base_url, created_at, updated_at
		FROM connection_profiles WHERE active = 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

// SetProfileBridge records the target bridge for an edge entry (v1.6
// 入口②: an edge may front several bridges; the choice belongs to the
// connection profile). Stored as an internal preference row keyed by the
// base URL — deliberately outside the Wails preference surface.
func (s *Store) SetProfileBridge(ctx context.Context, rawURL, bridgeID string) error {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return err
	}
	if bridgeID != "" && len(bridgeID) > 128 {
		return fmt.Errorf("bridge id too long")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		profileBridgeKey(baseURL), bridgeID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set profile bridge: %w", err)
	}
	return nil
}

// ProfileBridge returns the profile's target bridge ("" when unset —
// direct-bridge entries and embedded profiles).
func (s *Store) ProfileBridge(ctx context.Context, rawURL string) string {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return ""
	}
	var value string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, profileBridgeKey(baseURL)).Scan(&value)
	if err != nil {
		return ""
	}
	if len(value) > 128 {
		return ""
	}
	return value
}

func profileBridgeKey(baseURL string) string {
	return "connection.bridge." + baseURL
}

// SetProfileRelayToken records the relay session token for a relay
// profile (the relay login's body token; design §3.3). Stored as an
// internal preference keyed by base URL, mirroring the bridge pin — no
// schema change, the preferences table is generic.
func (s *Store) SetProfileRelayToken(ctx context.Context, rawURL, token string) error {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("relay token is required")
	}
	if len(token) > 512 {
		return fmt.Errorf("relay token too long")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		profileRelayTokenKey(baseURL), token, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set profile relay token: %w", err)
	}
	return nil
}

// ProfileRelayToken returns the profile's relay session token ("" when
// unset — direct-bridge, embedded, and enterprise profiles).
func (s *Store) ProfileRelayToken(ctx context.Context, rawURL string) string {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return ""
	}
	var value string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, profileRelayTokenKey(baseURL)).Scan(&value)
	if err != nil {
		return ""
	}
	if len(value) > 512 {
		return ""
	}
	return value
}

// DeleteProfileRelayToken removes the profile's relay session token row —
// the standalone delete used by Logout and by the direct-bridge connect
// paths (DeleteProfile already removes the row inside its transaction).
// Deleting an unset row is a no-op.
func (s *Store) DeleteProfileRelayToken(ctx context.Context, rawURL string) error {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM preferences WHERE key = ?`, profileRelayTokenKey(baseURL)); err != nil {
		return fmt.Errorf("delete profile relay token: %w", err)
	}
	return nil
}

func profileRelayTokenKey(baseURL string) string {
	return "connection.relay-token." + baseURL
}

// ProfileRow is one profile plus its session/bookkeeping columns for the
// connect page's saved-connection list (LEFT JOIN shape).
type ProfileRow struct {
	Profile
	Active           bool
	HasSession       bool
	SessionExpiresAt int64 // UnixMilli; 0 when HasSession is false
}

// ListProfiles returns every saved profile with its session columns,
// newest-touched first. The connect page renders the active-session ones
// on top; the raw order here is by updated_at so callers can re-sort.
func (s *Store) ListProfiles(ctx context.Context) ([]ProfileRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.base_url, p.created_at, p.updated_at, p.active,
		       (n.profile_id IS NOT NULL), COALESCE(n.expires_at, 0)
		FROM connection_profiles p
		LEFT JOIN native_sessions n ON n.profile_id = p.id
		ORDER BY p.updated_at DESC, p.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list connection profiles: %w", err)
	}
	defer rows.Close()
	var out []ProfileRow
	for rows.Next() {
		var row ProfileRow
		var active int
		if err := rows.Scan(&row.ID, &row.BaseURL, &row.CreatedAt, &row.UpdatedAt, &active, &row.HasSession, &row.SessionExpiresAt); err != nil {
			return nil, fmt.Errorf("scan connection profile: %w", err)
		}
		row.Active = active == 1
		out = append(out, row)
	}
	return out, rows.Err()
}

// ProfileByID loads one profile regardless of active state.
func (s *Store) ProfileByID(ctx context.Context, id int64) (Profile, bool, error) {
	profile, err := scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, base_url, created_at, updated_at
		FROM connection_profiles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

// ActivateProfile makes the given profile the active one (the profile
// Bootstrap resumes on next start). Mirrors SaveProfile's exclusive-active
// contract.
func (s *Store) ActivateProfile(ctx context.Context, id int64) (Profile, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Profile{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE connection_profiles SET active = 0 WHERE active = 1`); err != nil {
		return Profile{}, false, fmt.Errorf("clear active connection profile: %w", err)
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `UPDATE connection_profiles SET active = 1, updated_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return Profile{}, false, fmt.Errorf("activate connection profile: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Profile{}, false, nil
	}
	profile, err := scanProfile(tx.QueryRowContext(ctx, `
		SELECT id, base_url, created_at, updated_at
		FROM connection_profiles WHERE id = ?`, id))
	if err != nil {
		return Profile{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

// DeleteProfile removes a profile, its sealed session (FK cascade), and
// its bridge-pin preference. Returns false when the id is unknown.
func (s *Store) DeleteProfile(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var baseURL string
	err = tx.QueryRowContext(ctx, `SELECT base_url FROM connection_profiles WHERE id = ?`, id).Scan(&baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences WHERE key = ?`, profileBridgeKey(baseURL)); err != nil {
		return false, fmt.Errorf("delete profile bridge pin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences WHERE key = ?`, profileRelayTokenKey(baseURL)); err != nil {
		return false, fmt.Errorf("delete profile relay token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_profiles WHERE id = ?`, id); err != nil {
		return false, fmt.Errorf("delete connection profile: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Preference(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// PutPreferenceVersioned claims this Store's epoch lazily, then atomically
// applies a preference only while the Store still owns the latest persisted
// epoch and revision is newer for the key. Epoch fencing makes ordering
// authoritative in SQLite even when another process bypasses Wails'
// best-effort single-instance lock.
func (s *Store) PutPreferenceVersioned(ctx context.Context, key, value string, revision uint64) (bool, error) {
	if revision == 0 || revision > math.MaxInt64 {
		return false, fmt.Errorf("preference revision is invalid")
	}
	s.preferenceWriteMu.Lock()
	defer s.preferenceWriteMu.Unlock()
	if s.epoch == 0 {
		epoch, err := s.claimPreferenceEpoch(ctx)
		if err != nil {
			return false, err
		}
		s.epoch = epoch
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("reserve SQLite connection for preference write: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, fmt.Errorf("begin versioned preference write: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var currentEpoch int64
	if err := conn.QueryRowContext(ctx, `SELECT epoch FROM preference_epoch WHERE singleton = 1`).Scan(&currentEpoch); err != nil {
		return false, fmt.Errorf("read current preference epoch: %w", err)
	}
	if currentEpoch != s.epoch {
		return false, nil
	}

	var appliedEpoch, appliedRevision int64
	err = conn.QueryRowContext(ctx, `
		SELECT epoch, revision FROM preference_versions WHERE key = ?`, key).
		Scan(&appliedEpoch, &appliedRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read applied preference version: %w", err)
	}
	if err == nil {
		if appliedEpoch > s.epoch {
			return false, fmt.Errorf("stored preference version is invalid")
		}
		if appliedEpoch == s.epoch && appliedRevision >= int64(revision) {
			return false, nil
		}
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix()); err != nil {
		return false, fmt.Errorf("write preference value: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO preference_versions (key, epoch, revision) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET epoch = excluded.epoch, revision = excluded.revision`,
		key, s.epoch, int64(revision)); err != nil {
		return false, fmt.Errorf("write preference version: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("commit versioned preference write: %w", err)
	}
	committed = true
	return true, nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	migrationsExist, err := sqliteObjectExists(ctx, tx, "table", "schema_migrations")
	if err != nil {
		return err
	}
	if !migrationsExist {
		if err := rejectApplicationObjects(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, schemaMigrationsDDL); err != nil {
			return fmt.Errorf("create schema migrations table: %w", err)
		}
	}
	if err := validateColumns(ctx, tx, "schema_migrations", []schemaColumn{
		{name: "version", kind: "INTEGER", primaryKey: true},
		{name: "applied_at", kind: "INTEGER", notNull: true},
	}); err != nil {
		return err
	}
	if err := validateCanonicalDDL(ctx, tx, "table", "schema_migrations", schemaMigrationsDDL); err != nil {
		return err
	}

	version, versioned, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	if version == currentSchemaVersion {
		if err := validateVersionTwoSchema(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if versioned {
		if version != 1 {
			return fmt.Errorf("unsupported database schema version %d", version)
		}
		if err := validateVersionOneSchema(ctx, tx); err != nil {
			return err
		}
		if err := applyVersionTwoMigration(ctx, tx); err != nil {
			return err
		}
		if err := validateVersionTwoSchema(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, currentSchemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("record SQLite migration: %w", err)
		}
		return tx.Commit()
	}
	if err := rejectApplicationObjects(ctx, tx); err != nil {
		return err
	}

	for _, statement := range []string{
		connectionProfilesDDL,
		oneActiveProfileDDL,
		preferencesDDL,
		nativeSessionsDDL,
		preferenceEpochDDL,
		preferenceVersionsDDL,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply SQLite migration: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO preference_epoch (singleton, epoch) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize preference epoch: %w", err)
	}
	if err := validateVersionTwoSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, currentSchemaVersion, time.Now().Unix()); err != nil {
		return fmt.Errorf("record SQLite migration: %w", err)
	}
	return tx.Commit()
}

func applyVersionTwoMigration(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{preferenceEpochDDL, preferenceVersionsDDL} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply SQLite migration version 2: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO preference_epoch (singleton, epoch) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize preference epoch: %w", err)
	}
	return nil
}

func (s *Store) claimPreferenceEpoch(ctx context.Context) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve SQLite connection for preference epoch: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, fmt.Errorf("begin preference epoch claim: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	result, err := conn.ExecContext(ctx, `
		UPDATE preference_epoch SET epoch = epoch + 1
		WHERE singleton = 1 AND epoch < ?`, int64(math.MaxInt64))
	if err != nil {
		return 0, fmt.Errorf("claim preference epoch: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read preference epoch claim result: %w", err)
	}
	if changed != 1 {
		return 0, fmt.Errorf("preference epoch is unavailable")
	}
	var epoch int64
	if err := conn.QueryRowContext(ctx, `SELECT epoch FROM preference_epoch WHERE singleton = 1`).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("read claimed preference epoch: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, fmt.Errorf("commit preference epoch claim: %w", err)
	}
	committed = true
	return epoch, nil
}

func schemaVersion(ctx context.Context, tx *sql.Tx) (int, bool, error) {
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, false, fmt.Errorf("read database schema version: %w", err)
	}
	if !version.Valid {
		return 0, false, nil
	}
	return int(version.Int64), true, nil
}

func rejectApplicationObjects(ctx context.Context, tx *sql.Tx) error {
	const query = `SELECT name FROM sqlite_master WHERE lower(name) IN ('connection_profiles', 'preferences', 'native_sessions', 'one_active_profile', 'preference_epoch', 'preference_versions') LIMIT 1`
	var name string
	err := tx.QueryRowContext(ctx, query).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect application schema objects: %w", err)
	}
	return fmt.Errorf("unversioned database contains application object %q", name)
}

func sqliteObjectExists(ctx context.Context, tx *sql.Tx, objectType, name string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND lower(name) = lower(?)`, objectType, name).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect SQLite object %s: %w", name, err)
	}
	return count == 1, nil
}

type schemaColumn struct {
	name       string
	kind       string
	notNull    bool
	primaryKey bool
}

type schemaColumnInfo struct {
	kind       string
	notNull    bool
	primaryKey bool
}

func validateVersionOneSchema(ctx context.Context, tx *sql.Tx) error {
	for _, object := range []struct {
		objectType string
		name       string
		definition string
	}{
		{"table", "connection_profiles", connectionProfilesDDL},
		{"index", "one_active_profile", oneActiveProfileDDL},
		{"table", "preferences", preferencesDDL},
		{"table", "native_sessions", nativeSessionsDDL},
	} {
		if err := validateCanonicalDDL(ctx, tx, object.objectType, object.name, object.definition); err != nil {
			return err
		}
	}

	for table, columns := range map[string][]schemaColumn{
		"connection_profiles": {
			{name: "id", kind: "INTEGER", primaryKey: true},
			{name: "base_url", kind: "TEXT", notNull: true},
			{name: "active", kind: "INTEGER", notNull: true},
			{name: "created_at", kind: "INTEGER", notNull: true},
			{name: "updated_at", kind: "INTEGER", notNull: true},
		},
		"preferences": {
			{name: "key", kind: "TEXT", primaryKey: true},
			{name: "value", kind: "TEXT", notNull: true},
			{name: "updated_at", kind: "INTEGER", notNull: true},
		},
		"native_sessions": {
			{name: "profile_id", kind: "INTEGER", primaryKey: true},
			{name: "device_id", kind: "TEXT", notNull: true},
			{name: "session_ciphertext", kind: "BLOB", notNull: true},
			{name: "expires_at", kind: "INTEGER", notNull: true},
			{name: "last_validated_at", kind: "INTEGER", notNull: true},
		},
	} {
		if err := validateColumns(ctx, tx, table, columns); err != nil {
			return err
		}
	}
	if err := validateConnectionProfileSemantics(ctx, tx); err != nil {
		return err
	}
	if err := validateNativeSessionForeignKey(ctx, tx); err != nil {
		return err
	}

	var indexSQL sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND lower(name) = 'one_active_profile'`).Scan(&indexSQL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("incompatible schema: missing one_active_profile index")
		}
		return fmt.Errorf("read one_active_profile index: %w", err)
	}
	normalizedIndex := strings.Join(strings.Fields(strings.ToLower(indexSQL.String)), "")
	if normalizedIndex != "createuniqueindexone_active_profileonconnection_profiles(active)whereactive=1" {
		return fmt.Errorf("incompatible schema: invalid one_active_profile index")
	}
	var unique, partial int
	if err := tx.QueryRowContext(ctx, `SELECT "unique", partial FROM pragma_index_list(?) WHERE lower(name) = 'one_active_profile'`, "connection_profiles").Scan(&unique, &partial); err != nil {
		return fmt.Errorf("inspect one_active_profile index: %w", err)
	}
	if unique != 1 || partial != 1 {
		return fmt.Errorf("incompatible schema: one_active_profile index must be unique and partial")
	}
	if err := validateIndexColumns(ctx, tx, "one_active_profile", []string{"active"}); err != nil {
		return err
	}
	return nil
}

func validateVersionTwoSchema(ctx context.Context, tx *sql.Tx) error {
	if err := validateVersionOneSchema(ctx, tx); err != nil {
		return err
	}
	for _, object := range []struct {
		name       string
		definition string
	}{
		{"preference_epoch", preferenceEpochDDL},
		{"preference_versions", preferenceVersionsDDL},
	} {
		if err := validateCanonicalDDL(ctx, tx, "table", object.name, object.definition); err != nil {
			return err
		}
	}
	for table, columns := range map[string][]schemaColumn{
		"preference_epoch": {
			{name: "singleton", kind: "INTEGER", primaryKey: true},
			{name: "epoch", kind: "INTEGER", notNull: true},
		},
		"preference_versions": {
			{name: "key", kind: "TEXT", primaryKey: true},
			{name: "epoch", kind: "INTEGER", notNull: true},
			{name: "revision", kind: "INTEGER", notNull: true},
		},
	} {
		if err := validateColumns(ctx, tx, table, columns); err != nil {
			return err
		}
	}
	if err := validatePreferenceVersionForeignKey(ctx, tx); err != nil {
		return err
	}
	return validatePreferenceOrderingState(ctx, tx)
}

func validatePreferenceVersionForeignKey(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT "table", "from", "to", on_update, on_delete, "match" FROM pragma_foreign_key_list(?)`, "preference_versions")
	if err != nil {
		return fmt.Errorf("inspect preference_versions foreign keys: %w", err)
	}
	defer rows.Close()
	var foreignKeys int
	for rows.Next() {
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return fmt.Errorf("read preference_versions foreign key: %w", err)
		}
		foreignKeys++
		if !strings.EqualFold(table, "preferences") || from != "key" || to != "key" || !strings.EqualFold(onUpdate, "NO ACTION") || !strings.EqualFold(onDelete, "CASCADE") || !strings.EqualFold(match, "NONE") {
			return fmt.Errorf("incompatible schema: preference_versions has an unexpected foreign key")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read preference_versions foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("incompatible schema: preference_versions must have exactly one preference foreign key")
	}
	return nil
}

func validatePreferenceOrderingState(ctx context.Context, tx *sql.Tx) error {
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM preference_epoch WHERE singleton = 1`).Scan(&epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("incompatible schema: preference epoch singleton is missing")
		}
		return fmt.Errorf("read preference epoch: %w", err)
	}
	if epoch < 0 {
		return fmt.Errorf("incompatible schema: preference epoch is negative")
	}
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM preference_epoch`).Scan(&rows); err != nil {
		return fmt.Errorf("count preference epochs: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("incompatible schema: preference epoch must contain one singleton")
	}
	var invalidVersions int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM preference_versions
		WHERE epoch <= 0 OR revision <= 0 OR epoch > ?`, epoch).Scan(&invalidVersions); err != nil {
		return fmt.Errorf("validate preference versions: %w", err)
	}
	if invalidVersions != 0 {
		return fmt.Errorf("incompatible schema: preference versions contain invalid ordering data")
	}
	return nil
}

func validateCanonicalDDL(ctx context.Context, tx *sql.Tx, objectType, name, want string) error {
	got, err := sqliteObjectSQL(ctx, tx, objectType, name)
	if err != nil {
		return err
	}
	if normalizeSQLiteSQL(got) != normalizeSQLiteSQL(want) {
		return fmt.Errorf("incompatible schema: %s %s does not match its canonical definition", objectType, name)
	}
	return nil
}

func validateConnectionProfileSemantics(ctx context.Context, tx *sql.Tx) error {
	sqlDefinition, err := sqliteObjectSQL(ctx, tx, "table", "connection_profiles")
	if err != nil {
		return err
	}
	normalized := normalizeSQLiteSQL(sqlDefinition)
	for _, required := range []string{
		"idintegerprimarykeyautoincrement",
		"activeintegernotnulldefault0check(activein(0,1))",
	} {
		if !strings.Contains(normalized, required) {
			return fmt.Errorf("incompatible schema: connection_profiles is missing %s", required)
		}
	}
	return validateUniqueIndex(ctx, tx, "connection_profiles", []string{"base_url"})
}

func validateNativeSessionForeignKey(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT "table", "from", "to", on_update, on_delete, "match" FROM pragma_foreign_key_list(?)`, "native_sessions")
	if err != nil {
		return fmt.Errorf("inspect native_sessions foreign keys: %w", err)
	}
	defer rows.Close()
	var foreignKeys int
	for rows.Next() {
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return fmt.Errorf("read native_sessions foreign key: %w", err)
		}
		foreignKeys++
		if !strings.EqualFold(table, "connection_profiles") || from != "profile_id" || to != "id" || !strings.EqualFold(onUpdate, "NO ACTION") || !strings.EqualFold(onDelete, "CASCADE") || !strings.EqualFold(match, "NONE") {
			return fmt.Errorf("incompatible schema: native_sessions has an unexpected foreign key")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read native_sessions foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("incompatible schema: native_sessions must have exactly one profile foreign key")
	}
	return nil
}

func sqliteObjectSQL(ctx context.Context, tx *sql.Tx, objectType, name string) (string, error) {
	var definition sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = ? AND lower(name) = lower(?)`, objectType, name).Scan(&definition); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("incompatible schema: missing %s %s", objectType, name)
		}
		return "", fmt.Errorf("read %s %s: %w", objectType, name, err)
	}
	return definition.String, nil
}

func validateUniqueIndex(ctx context.Context, tx *sql.Tx, table string, columns []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name, "unique", partial FROM pragma_index_list(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s indexes: %w", table, err)
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var name string
		var unique, partial int
		if err := rows.Scan(&name, &unique, &partial); err != nil {
			return fmt.Errorf("read %s index: %w", table, err)
		}
		if unique == 1 && partial == 0 {
			candidates = append(candidates, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s indexes: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s indexes: %w", table, err)
	}
	for _, name := range candidates {
		if validateIndexColumns(ctx, tx, name, columns) == nil {
			return nil
		}
	}
	return fmt.Errorf("incompatible schema: %s must have a unique %s index", table, strings.Join(columns, ", "))
}

func validateIndexColumns(ctx context.Context, tx *sql.Tx, name string, want []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
	if err != nil {
		return fmt.Errorf("inspect %s index columns: %w", name, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return fmt.Errorf("read %s index columns: %w", name, err)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s index columns: %w", name, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("incompatible schema: %s index has unexpected columns", name)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("incompatible schema: %s index has unexpected columns", name)
		}
	}
	return nil
}

func normalizeSQLiteSQL(definition string) string {
	return strings.Join(strings.Fields(strings.ToLower(definition)), "")
}

func validateColumns(ctx context.Context, tx *sql.Tx, table string, want []schemaColumn) error {
	rows, err := tx.QueryContext(ctx, `SELECT name, type, "notnull", pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()

	got := make(map[string]schemaColumnInfo, len(want))
	for rows.Next() {
		var name, kind string
		var notNull, primaryKey int
		if err := rows.Scan(&name, &kind, &notNull, &primaryKey); err != nil {
			return fmt.Errorf("read %s schema: %w", table, err)
		}
		got[name] = schemaColumnInfo{kind: strings.ToUpper(kind), notNull: notNull != 0, primaryKey: primaryKey != 0}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s schema: %w", table, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("incompatible schema: %s has %d columns, want %d", table, len(got), len(want))
	}
	for _, column := range want {
		actual, ok := got[column.name]
		if !ok || actual.kind != column.kind || actual.notNull != column.notNull || actual.primaryKey != column.primaryKey {
			return fmt.Errorf("incompatible schema: %s.%s has unexpected definition", table, column.name)
		}
	}
	return nil
}

type profileScanner interface {
	Scan(dest ...any) error
}

func scanProfile(row profileScanner) (Profile, error) {
	var profile Profile
	if err := row.Scan(&profile.ID, &profile.BaseURL, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func NormalizeBaseURL(rawURL string) (string, error) {
	if containsDefaultIgnorable(rawURL) {
		return "", fmt.Errorf("server URL contains forbidden characters")
	}
	if strings.Contains(rawAuthority(rawURL), "%") {
		return "", fmt.Errorf("server URL contains forbidden characters")
	}
	if decoded, err := url.PathUnescape(rawURL); err == nil && containsDefaultIgnorable(decoded) {
		return "", fmt.Errorf("server URL contains forbidden characters")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("server URL is invalid")
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("server URL must include scheme and host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(rawURL, "#") {
		return "", fmt.Errorf("server URL cannot include credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" {
		return "", fmt.Errorf("server URL must be an origin")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("server URL must use HTTPS")
	}
	host, port, hasPort, err := normalizeAuthority(parsed)
	if err != nil {
		return "", err
	}
	if scheme == "http" && !isLoopbackHost(strings.Trim(host, "[]")) {
		return "", fmt.Errorf("HTTP is only allowed for loopback server URLs")
	}
	if hasPort && !isDefaultPort(scheme, port) {
		host += ":" + strconv.Itoa(port)
	}
	return scheme + "://" + host, nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

func normalizeAuthority(parsed *url.URL) (string, int, bool, error) {
	authority := parsed.Host
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", 0, false, fmt.Errorf("server URL must include a host")
	}
	if containsDefaultIgnorable(authority) || containsDefaultIgnorable(hostname) {
		return "", 0, false, fmt.Errorf("server URL contains forbidden characters")
	}
	hasPort := hasExplicitPort(authority)
	port := 0
	if hasPort {
		rawPort := parsed.Port()
		if rawPort == "" {
			return "", 0, false, fmt.Errorf("server URL port is required after colon")
		}
		for _, character := range rawPort {
			if character < '0' || character > '9' {
				return "", 0, false, fmt.Errorf("server URL port must be numeric")
			}
		}
		var err error
		port, err = strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return "", 0, false, fmt.Errorf("server URL port must be between 1 and 65535")
		}
	}

	if strings.Contains(hostname, "%") || strings.Contains(authority, "%") {
		return "", 0, false, fmt.Errorf("server URL must not include an IPv6 zone")
	}
	bracketed := strings.HasPrefix(authority, "[")
	if bracketed {
		address, err := netip.ParseAddr(hostname)
		if err != nil || !address.Is6() {
			return "", 0, false, fmt.Errorf("only IPv6 server URL hosts may be bracketed")
		}
		return "[" + address.String() + "]", port, hasPort, nil
	}

	mappedHostname, err := idna.Lookup.ToASCII(hostname)
	if err != nil {
		return "", 0, false, fmt.Errorf("server URL host is invalid")
	}
	mappedHostname = strings.ToLower(mappedHostname)
	if address, err := netip.ParseAddr(mappedHostname); err == nil {
		if address.Is6() {
			return "", 0, false, fmt.Errorf("IPv6 server URL hosts must be bracketed")
		}
		if hostname != address.String() {
			return "", 0, false, fmt.Errorf("server URL IPv4 address is not canonical")
		}
		return address.String(), port, hasPort, nil
	}
	if isNumericLookingIPv4(mappedHostname) {
		return "", 0, false, fmt.Errorf("server URL IPv4 address is not canonical")
	}
	if strings.ContainsAny(mappedHostname, "[]") || strings.Contains(mappedHostname, ":") {
		return "", 0, false, fmt.Errorf("server URL host is malformed")
	}
	return mappedHostname, port, hasPort, nil
}

func hasExplicitPort(authority string) bool {
	if strings.HasPrefix(authority, "[") {
		closingBracket := strings.LastIndex(authority, "]")
		return closingBracket >= 0 && len(authority) > closingBracket+1
	}
	return strings.Contains(authority, ":")
}

func isDefaultPort(scheme string, port int) bool {
	return scheme == "https" && port == 443 || scheme == "http" && port == 80
}

func containsDefaultIgnorable(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r) {
			return true
		}
	}
	return false
}

func rawAuthority(rawURL string) string {
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd < 0 {
		return ""
	}
	authority := rawURL[schemeEnd+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		return authority[:end]
	}
	return authority
}

func isNumericLookingIPv4(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	components := strings.Split(host, ".")
	for _, component := range components {
		if !isIPv4NumberComponent(component) {
			return false
		}
	}
	return true
}

func isIPv4NumberComponent(component string) bool {
	if component == "" {
		return false
	}
	digits := component
	base := 10
	if len(component) > 2 && strings.HasPrefix(component, "0x") {
		base = 16
		digits = component[2:]
	}
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r >= '0' && r <= '9' && (base == 10 || int(r-'0') < base) {
			continue
		}
		if base == 16 && r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

// NativeSession is the persisted (encrypted) opaque session of a profile.
// The ciphertext is opaque to this store; only the device key can open it.
type NativeSession struct {
	ProfileID         int64
	DeviceID          string
	SessionCiphertext []byte
	ExpiresAt         int64
	LastValidatedAt   int64
}

// SaveNativeSession upserts the encrypted session bound to a profile.
func (s *Store) SaveNativeSession(ctx context.Context, profileID int64, deviceID string, ciphertext []byte, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO native_sessions (profile_id, device_id, session_ciphertext, expires_at, last_validated_at)
		VALUES (?, ?, ?, ?, 0)
		ON CONFLICT(profile_id) DO UPDATE SET
		  device_id = excluded.device_id,
		  session_ciphertext = excluded.session_ciphertext,
		  expires_at = excluded.expires_at,
		  last_validated_at = 0`,
		profileID, deviceID, ciphertext, expiresAt)
	if err != nil {
		return fmt.Errorf("configstore: save native session: %w", err)
	}
	return nil
}

// NativeSession returns the stored session of a profile, if any.
func (s *Store) NativeSession(ctx context.Context, profileID int64) (NativeSession, bool, error) {
	var n NativeSession
	err := s.db.QueryRowContext(ctx,
		`SELECT profile_id, device_id, session_ciphertext, expires_at, last_validated_at
		 FROM native_sessions WHERE profile_id = ?`, profileID).
		Scan(&n.ProfileID, &n.DeviceID, &n.SessionCiphertext, &n.ExpiresAt, &n.LastValidatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NativeSession{}, false, nil
	}
	if err != nil {
		return NativeSession{}, false, fmt.Errorf("configstore: native session: %w", err)
	}
	return n, true, nil
}

// DeleteNativeSession removes the stored session (logout).
func (s *Store) DeleteNativeSession(ctx context.Context, profileID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM native_sessions WHERE profile_id = ?`, profileID); err != nil {
		return fmt.Errorf("configstore: delete native session: %w", err)
	}
	return nil
}

// TouchNativeSession records the last successful validation.
func (s *Store) TouchNativeSession(ctx context.Context, profileID int64, atMilli int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE native_sessions SET last_validated_at = ? WHERE profile_id = ?`, atMilli, profileID); err != nil {
		return fmt.Errorf("configstore: touch native session: %w", err)
	}
	return nil
}
