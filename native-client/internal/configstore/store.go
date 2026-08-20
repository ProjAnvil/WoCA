package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	if versioned {
		if version != currentSchemaVersion {
			return fmt.Errorf("unsupported database schema version %d", version)
		}
		if err := validateVersionOneSchema(ctx, tx); err != nil {
			return err
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
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply SQLite migration: %w", err)
		}
	}
	if err := validateVersionOneSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, currentSchemaVersion, time.Now().Unix()); err != nil {
		return fmt.Errorf("record SQLite migration: %w", err)
	}
	return tx.Commit()
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
	const query = `SELECT name FROM sqlite_master WHERE lower(name) IN ('connection_profiles', 'preferences', 'native_sessions', 'one_active_profile') LIMIT 1`
	var name string
	err := tx.QueryRowContext(ctx, query).Scan(&name)
	if err == sql.ErrNoRows {
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
		if err == sql.ErrNoRows {
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

func validateCanonicalDDL(ctx context.Context, tx *sql.Tx, objectType, name, want string) error {
	got, err := sqliteObjectSQL(ctx, tx, objectType, name)
	if err != nil {
		return err
	}
	if normalizeSQLiteSQL(got) != normalizeSQLiteSQL(want) {
		return fmt.Errorf("incompatible schema: %s %s does not match version %d", objectType, name, currentSchemaVersion)
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
		if err == sql.ErrNoRows {
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
	if address, err := netip.ParseAddr(hostname); err == nil {
		if address.Is6() {
			if !bracketed {
				return "", 0, false, fmt.Errorf("IPv6 server URL hosts must be bracketed")
			}
			return "[" + address.String() + "]", port, hasPort, nil
		}
		if bracketed {
			return "", 0, false, fmt.Errorf("only IPv6 server URL hosts may be bracketed")
		}
		return address.String(), port, hasPort, nil
	}
	if bracketed {
		return "", 0, false, fmt.Errorf("only IPv6 server URL hosts may be bracketed")
	}
	if strings.ContainsAny(hostname, "[]") || strings.Contains(hostname, ":") {
		return "", 0, false, fmt.Errorf("server URL host is malformed")
	}
	return strings.ToLower(hostname), port, hasPort, nil
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
