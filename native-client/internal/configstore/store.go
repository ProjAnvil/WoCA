package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Profile struct {
	ID        int64  `json:"id"`
	BaseURL   string `json:"base_url"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Store struct {
	db *sql.DB
}

const currentSchemaVersion = 1

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
	if err == sql.ErrNoRows {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

func (s *Store) PutPreference(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

func (s *Store) Preference(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
            version INTEGER PRIMARY KEY,
            applied_at INTEGER NOT NULL
        )`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	if err := validateColumns(ctx, tx, "schema_migrations", []schemaColumn{
		{name: "version", kind: "INTEGER", primaryKey: true},
		{name: "applied_at", kind: "INTEGER", notNull: true},
	}); err != nil {
		return err
	}

	version, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}

	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS connection_profiles (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            base_url TEXT NOT NULL UNIQUE,
            active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
            created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL
        )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_active_profile
            ON connection_profiles(active) WHERE active = 1`,
		`CREATE TABLE IF NOT EXISTS preferences (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL,
            updated_at INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS native_sessions (
            profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
            device_id TEXT NOT NULL,
            session_ciphertext BLOB NOT NULL,
            expires_at INTEGER NOT NULL,
            last_validated_at INTEGER NOT NULL
        )`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply SQLite migration: %w", err)
		}
	}
	if err := validateVersionOneSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)`, currentSchemaVersion, time.Now().Unix()); err != nil {
		return fmt.Errorf("record SQLite migration: %w", err)
	}
	return tx.Commit()
}

func schemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read database schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
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

	var indexSQL sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'one_active_profile'`).Scan(&indexSQL); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("incompatible schema: missing one_active_profile index")
		}
		return fmt.Errorf("read one_active_profile index: %w", err)
	}
	normalizedIndex := strings.Join(strings.Fields(strings.ToLower(indexSQL.String)), "")
	if !strings.Contains(normalizedIndex, "onconnection_profiles(active)whereactive=1") {
		return fmt.Errorf("incompatible schema: invalid one_active_profile index")
	}
	var unique, partial int
	if err := tx.QueryRowContext(ctx, `SELECT "unique", partial FROM pragma_index_list(?) WHERE name = ?`, "connection_profiles", "one_active_profile").Scan(&unique, &partial); err != nil {
		return fmt.Errorf("inspect one_active_profile index: %w", err)
	}
	if unique != 1 || partial != 1 {
		return fmt.Errorf("incompatible schema: one_active_profile index must be unique and partial")
	}
	return nil
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
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
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
	host := strings.ToLower(parsed.Host)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("server URL must use HTTPS")
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("HTTP is only allowed for loopback server URLs")
	}
	return scheme + "://" + host, nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}
