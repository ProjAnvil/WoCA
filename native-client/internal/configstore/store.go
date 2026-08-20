package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"net"
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
	baseURL, err := normalizeBaseURL(rawURL)
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

	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
            version INTEGER PRIMARY KEY,
            applied_at INTEGER NOT NULL
        )`,
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
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (1, ?)`, time.Now().Unix()); err != nil {
		return fmt.Errorf("record SQLite migration: %w", err)
	}
	return tx.Commit()
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

func normalizeBaseURL(rawURL string) (string, error) {
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
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
