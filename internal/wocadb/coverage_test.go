package wocadb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCryptoLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// KeyPath reflects the isolated home.
	if got := KeyPath(); got != filepath.Join(home, ".woca", "secret.key") {
		t.Fatalf("KeyPath = %q", got)
	}

	// Creating the key persists it; a reload yields the same crypto.
	path := filepath.Join(home, ".woca", "secret.key")
	c1, err := LoadOrCreateCrypto(path)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreateCrypto(path)
	if err != nil {
		t.Fatal(err)
	}

	ct, err := c1.Encrypt("secret-value")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := c2.Decrypt(ct)
	if err != nil || pt != "secret-value" {
		t.Fatalf("round trip = %q %v", pt, err)
	}

	// Empty key path falls back to the default location.
	c3, err := LoadOrCreateCrypto("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c3.Encrypt("x"); err != nil {
		t.Fatal(err)
	}

	// Error branches.
	if _, err := newCrypto([]byte("short")); err == nil {
		t.Error("short key should fail")
	}
	if _, err := c1.Decrypt([]byte("tiny")); err == nil {
		t.Error("short ciphertext should fail")
	}
	if _, err := c1.Decrypt([]byte("0123456789abcdef0123456789abcdef")); err == nil {
		t.Error("tampered ciphertext should fail")
	}

	// An unwritable key directory fails at create time.
	blocker := filepath.Join(home, "blocker")
	if err := writeBlocker(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCrypto(filepath.Join(blocker, "secret.key")); err == nil {
		t.Error("expected key dir failure")
	}
	// A directory in place of the key file fails at read time.
	dir := filepath.Join(home, "adir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCrypto(dir); err == nil {
		t.Error("expected key read failure")
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

func writeBlocker(path string) error {
	return os.WriteFile(path, []byte("x"), 0o644)
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
