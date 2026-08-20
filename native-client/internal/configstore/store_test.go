package configstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePersistsActiveProfileAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SaveProfile(t.Context(), "https://Enterprise.Example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://enterprise.example.com" {
		t.Fatalf("BaseURL = %q", got.BaseURL)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active, ok, err := s.ActiveProfile(t.Context())
	if err != nil || !ok {
		t.Fatalf("ActiveProfile = %#v, %v, %v", active, ok, err)
	}
	if active.ID != got.ID || active.BaseURL != got.BaseURL {
		t.Fatalf("active = %#v, want %#v", active, got)
	}
}

func TestStorePersistsPreference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutPreference(t.Context(), "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	value, ok, err := s.Preference(t.Context(), "theme")
	if err != nil || !ok || value != "dark" {
		t.Fatalf("Preference() = %q, %v, %v", value, ok, err)
	}
}

func TestStoreRejectsURLWithCredentialsQueryOrFragment(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, rawURL := range []string{
		"https://u:p@example.com",
		"https://example.com?q=1",
		"https://example.com/#x",
		"https://example.com/app",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := s.SaveProfile(t.Context(), rawURL); err == nil {
				t.Fatalf("SaveProfile(%q) error = nil", rawURL)
			}
		})
	}

	_, ok, err := s.ActiveProfile(t.Context())
	if err != nil || ok {
		t.Fatalf("ActiveProfile() ok = %v, err = %v", ok, err)
	}
}

func TestStoreAllowsHTTPOnlyForLoopback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, rawURL := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		t.Run(rawURL, func(t *testing.T) {
			profile, err := s.SaveProfile(t.Context(), rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if profile.BaseURL != rawURL {
				t.Fatalf("BaseURL = %q, want %q", profile.BaseURL, rawURL)
			}
		})
	}

	if _, err := s.SaveProfile(t.Context(), "http://example.com"); err == nil {
		t.Fatal("SaveProfile(remote HTTP) error = nil")
	}
}

func TestNormalizeBaseURLAllowsOnlyExactHTTPLoopbackHosts(t *testing.T) {
	for rawURL, want := range map[string]string{
		"http://LOCALHOST:8080": "http://localhost:8080",
		"http://127.0.0.1:8080": "http://127.0.0.1:8080",
		"http://[::1]:8080":     "http://[::1]:8080",
	} {
		got, err := NormalizeBaseURL(rawURL)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q, nil", rawURL, got, err, want)
		}
	}

	for _, rawURL := range []string{
		"http://127.0.0.2:8080",
		"http://127.255.255.255:8080",
		"http://[::2]:8080",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}
}

func TestNormalizeBaseURLCanonicalizesAuthorityAndPorts(t *testing.T) {
	for rawURL, want := range map[string]string{
		"https://EXAMPLE.COM:443/":       "https://example.com",
		"https://EXAMPLE.COM:8443/":      "https://example.com:8443",
		"http://LOCALHOST:80/":           "http://localhost",
		"http://127.0.0.1:8080/":         "http://127.0.0.1:8080",
		"http://[0:0:0:0:0:0:0:1]:8080/": "http://[::1]:8080",
		"http://[::1]:80/":               "http://[::1]",
	} {
		got, err := NormalizeBaseURL(rawURL)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q, nil", rawURL, got, err, want)
		}
	}

	for _, rawURL := range []string{
		"https://example.com:",
		"https://example.com:0",
		"https://example.com:65536",
		"https://example.com:not-a-port",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}
}

func TestOpenRejectsFutureSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (2, 0);
	`)

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for future schema version")
	}
	if countApplicationObjects(t, path) != 0 {
		t.Fatal("Open() created application objects for future schema")
	}
}

func TestOpenRejectsIncompatibleVersionOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0);
		CREATE TABLE connection_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			base_url TEXT NOT NULL UNIQUE,
			active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
			created_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX one_active_profile ON connection_profiles(active) WHERE active = 1;
	`)

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for incompatible version-1 schema")
	}
}

func TestOpenDoesNotRepairVersionOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, strings.Replace(validVersionOneSchema, `CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`, "", 1))

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for incomplete version-1 schema")
	}
	if tableExists(t, path, "preferences") {
		t.Fatal("Open() repaired the incomplete version-1 schema")
	}
}

func TestOpenRejectsAlteredVersionOneSchemaSemantics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"base URL unique", func(schema string) string {
			return strings.Replace(schema, "base_url TEXT NOT NULL UNIQUE", "base_url TEXT NOT NULL", 1)
		}},
		{"active default", func(schema string) string {
			return strings.Replace(schema, "DEFAULT 0", "DEFAULT 1", 1)
		}},
		{"active check", func(schema string) string {
			return strings.Replace(schema, "CHECK (active IN (0, 1))", "CHECK (active IN (0, 2))", 1)
		}},
		{"profile autoincrement", func(schema string) string {
			return strings.Replace(schema, "INTEGER PRIMARY KEY AUTOINCREMENT", "INTEGER PRIMARY KEY", 1)
		}},
		{"native session foreign key", func(schema string) string {
			return strings.Replace(schema, "ON DELETE CASCADE", "ON DELETE RESTRICT", 1)
		}},
		{"active predicate", func(schema string) string {
			return strings.Replace(schema, "WHERE active = 1", "WHERE active IN (0, 1)", 1)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.db")
			prepareDatabase(t, path, tt.mutate(validVersionOneSchema))
			if store, err := Open(path); err == nil {
				store.Close()
				t.Fatal("Open() error = nil for altered version-1 schema")
			}
		})
	}
}

func prepareDatabase(t *testing.T, path, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
}

func countApplicationObjects(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('connection_profiles', 'preferences', 'native_sessions', 'one_active_profile')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func tableExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

const validVersionOneSchema = `
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0);
CREATE TABLE connection_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    base_url TEXT NOT NULL UNIQUE,
    active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX one_active_profile ON connection_profiles(active) WHERE active = 1;
CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE native_sessions (
    profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    session_ciphertext BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    last_validated_at INTEGER NOT NULL
);
`

func TestStoreSchemaHasNoAPIKeyOrOAuthTokenColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows, err := s.db.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		columns, err := s.db.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for columns.Next() {
			var column string
			if err := columns.Scan(&column); err != nil {
				columns.Close()
				t.Fatal(err)
			}
			normalized := strings.ToLower(strings.ReplaceAll(column, "-", "_"))
			for _, forbidden := range []string{"apikey", "api_key", "access_token", "refresh_token"} {
				if strings.Contains(normalized, forbidden) {
					columns.Close()
					t.Fatalf("%s.%s must not store %s", table, column, forbidden)
				}
			}
		}
		if err := columns.Err(); err != nil {
			columns.Close()
			t.Fatal(err)
		}
		columns.Close()
	}
}
