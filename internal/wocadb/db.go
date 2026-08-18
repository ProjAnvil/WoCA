// Package wocadb opens the woca settings database. SQLite (pure Go driver,
// stored under ~/.woca by default) is the primary backend; PostgreSQL is
// available via a postgres:// DSN for server deployments.
package wocadb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver name "pgx"
	_ "modernc.org/sqlite"             // database/sql driver name "sqlite"
)

// Dialect identifies the SQL flavor of an opened database.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// DB is an opened woca database with its dialect and human-readable location.
type DB struct {
	*sql.DB
	Dialect Dialect
	Path    string
}

// DefaultDir returns ~/.woca, the root of woca's on-disk state.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".woca"
	}
	return filepath.Join(home, ".woca")
}

// DefaultPath returns the default sqlite database location (~/.woca/woca.db).
func DefaultPath() string {
	return filepath.Join(DefaultDir(), "woca.db")
}

// Disabled reports whether target selects the "no database" escape hatch.
func Disabled(target string) bool {
	switch strings.TrimSpace(strings.ToLower(target)) {
	case "none", "off":
		return true
	}
	return false
}

// Open opens (and migrates) the woca database. Target resolution:
//
//	""                        → sqlite at ~/.woca/woca.db
//	"none" | "off"            → nil DB (settings disabled)
//	"sqlite"                  → sqlite at the default path
//	"sqlite:/path/to.db"      → sqlite at path
//	"/path/to.db" | "a.db"    → sqlite at path
//	"postgres://…" | "pg:…"   → PostgreSQL via pgx
func Open(target string) (*DB, error) {
	target = strings.TrimSpace(target)
	if Disabled(target) {
		return nil, nil
	}

	dialect, dsn, path := parseTarget(target)
	var (
		db  *sql.DB
		err error
	)
	switch dialect {
	case DialectPostgres:
		db, err = sql.Open("pgx", dsn)
	default:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("wocadb: create %s: %w", filepath.Dir(path), err)
		}
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	}
	if err != nil {
		return nil, fmt.Errorf("wocadb: open %s: %w", path, err)
	}
	if dialect == DialectSQLite {
		// A single connection avoids SQLITE_BUSY between concurrent writers
		// (checkpoint puts + settings updates) in one process.
		db.SetMaxOpenConns(1)
	}
	out := &DB{DB: db, Dialect: dialect, Path: path}
	if err := out.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return out, nil
}

// parseTarget splits a target into dialect, DSN, and display path.
func parseTarget(target string) (Dialect, string, string) {
	switch {
	case target == "" || strings.EqualFold(target, "sqlite"):
		return DialectSQLite, DefaultPath(), DefaultPath()
	case strings.HasPrefix(target, "sqlite:"):
		path := strings.TrimPrefix(target, "sqlite:")
		return DialectSQLite, path, path
	case strings.HasPrefix(target, "postgres://"), strings.HasPrefix(target, "postgresql://"),
		strings.HasPrefix(target, "pg:"):
		dsn := strings.TrimPrefix(target, "pg:")
		return DialectPostgres, dsn, summarizeDSN(dsn)
	default:
		// A bare path is treated as a sqlite file.
		return DialectSQLite, target, target
	}
}

// summarizeDSN reduces a DSN to scheme://host/db so it can be shown in the UI
// without leaking credentials.
func summarizeDSN(dsn string) string {
	if i := strings.IndexByte(dsn, '@'); i >= 0 && strings.Contains(dsn[:i], "://") {
		if j := strings.Index(dsn, "://"); j >= 0 && j < i {
			return dsn[:j+3] + dsn[i+1:]
		}
	}
	return dsn
}

// migrate creates the woca schema. All tables are keyed simply: settings and
// secrets are single-row-per-key; checkpoints store graph checkpoints;
// memory is a namespace+key JSON document store; session_state records the
// durable per-session run state (running / awaiting an interrupt answer).
func (d *DB) migrate() error {
	var stmts []string
	switch d.Dialect {
	case DialectPostgres:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secrets (
				name TEXT PRIMARY KEY,
				ciphertext BYTEA NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS checkpoints (
				key TEXT PRIMARY KEY,
				node TEXT NOT NULL,
				"values" TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS memory (
				namespace TEXT NOT NULL,
				key TEXT NOT NULL,
				value TEXT NOT NULL,
				updated_at BIGINT NOT NULL,
				PRIMARY KEY (namespace, key)
			)`,
			`CREATE TABLE IF NOT EXISTS session_state (
				session_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				run_id TEXT NOT NULL,
				owner TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
		}
	default:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secrets (
				name TEXT PRIMARY KEY,
				ciphertext BLOB NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS checkpoints (
				key TEXT PRIMARY KEY,
				node TEXT NOT NULL,
				"values" BLOB NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS memory (
				namespace TEXT NOT NULL,
				key TEXT NOT NULL,
				value BLOB NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (namespace, key)
			)`,
			`CREATE TABLE IF NOT EXISTS session_state (
				session_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				run_id TEXT NOT NULL,
				owner TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
		}
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("wocadb: migrate: %w", err)
		}
	}
	return nil
}

// Now returns the current unix timestamp in milliseconds.
func Now() int64 { return time.Now().UnixMilli() }
