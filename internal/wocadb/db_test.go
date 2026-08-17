package wocadb

import (
	"database/sql"
	"os"
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

func TestParseTargetVariants(t *testing.T) {
	cases := []struct {
		target             string
		wantDialect        Dialect
		wantSQLiteLikePath bool
		wantPostgres       bool
	}{
		{"file.db", DialectSQLite, true, false},
		{"/abs/path/x.db", DialectSQLite, true, false},
		{"postgres://u@h/db", DialectPostgres, false, true},
		{"postgresql://u@h/db", DialectPostgres, false, true},
		{"pg://u@h/db", DialectPostgres, false, true},
	}
	for _, c := range cases {
		dialect, _, path := parseTarget(c.target)
		if dialect != c.wantDialect {
			t.Errorf("parseTarget(%q) dialect = %v", c.target, dialect)
		}
		if c.wantSQLiteLikePath && path == "" {
			t.Errorf("parseTarget(%q) path empty", c.target)
		}
	}
}

func TestOpenVariantsAndNow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Plain relative name resolves next to the temp cwd; use absolute paths.
	p := filepath.Join(home, "a.db")
	db, err := Open(p)
	if err != nil || db == nil {
		t.Fatalf("Open = %v %v", db, err)
	}
	if db.Dialect != DialectSQLite || db.Path != p {
		t.Fatalf("db = %+v", db)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The sqlite: scheme form.
	db2, err := Open("sqlite:" + p)
	if err != nil || db2.Path != p {
		t.Fatalf("scheme form = %+v %v", db2, err)
	}
	db2.Close()

	// Disabled targets.
	if db3, err := Open("none"); err != nil || db3 != nil {
		t.Fatalf("none = %v %v", db3, err)
	}

	// A sqlite path under a regular file fails at directory creation.
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "x.db")); err == nil {
		t.Fatal("expected dir creation failure")
	}

	// DefaultDir without HOME is empty; Now is a positive millisecond clock.
	t.Setenv("HOME", "")
	if got := DefaultDir(); got != ".woca" {
		t.Fatalf("DefaultDir = %q", got)
	}
	if Now() < 1_700_000_000_000 {
		t.Fatalf("Now = %d", Now())
	}
}
