package wocadb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenSQLiteCreatesSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, table := range []string{"settings", "secrets", "checkpoints", "memory"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	if db.Dialect != DialectSQLite {
		t.Fatalf("dialect = %q, want sqlite", db.Dialect)
	}
}

func TestOpenDefaultUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := filepath.Join(home, ".woca", "woca.db")
	if db.Path != want {
		t.Fatalf("path = %q, want %q", db.Path, want)
	}
}

func TestOpenDisabled(t *testing.T) {
	db, err := Open("none")
	if err != nil || db != nil {
		t.Fatalf("Open(none) = %v, %v; want nil, nil", db, err)
	}
	if !Disabled("off") || Disabled("sqlite:x") {
		t.Fatal("Disabled misclassifies targets")
	}
}

func TestSummarizeDSNHidesCredentials(t *testing.T) {
	got := summarizeDSN("postgres://user:secret@host.example:5432/woca")
	want := "postgres://host.example:5432/woca"
	if got != want {
		t.Fatalf("summarizeDSN = %q, want %q", got, want)
	}
}

// TestSQLDriverRegistered guards the blank imports.
func TestSQLDriverRegistered(t *testing.T) {
	drivers := sql.Drivers()
	found := map[string]bool{}
	for _, d := range drivers {
		found[d] = true
	}
	if !found["sqlite"] {
		t.Fatal("sqlite driver not registered")
	}
	if !found["pgx"] {
		t.Fatal("pgx driver not registered")
	}
}
