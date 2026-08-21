package wocadb

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOpenPostgresMigrateConnectFailure drives the postgres dialect end to
// end against a refused loopback port: the pgx driver opens lazily, so Open
// itself succeeds and the failure surfaces on the first migrate statement.
// This still executes the whole postgres statement builder (tenant GUC
// defaults, lgcp schema, ownership policies) before the first Exec fails.
func TestOpenPostgresMigrateConnectFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	db, err := Open(fmt.Sprintf("postgres://woca:woca@127.0.0.1:%d/woca?sslmode=disable&connect_timeout=2", port))
	if err == nil {
		_ = db.Close()
		t.Fatal("Open against refused port should fail at migrate time")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("error should surface from migrate, got %v", err)
	}
}

// TestOpenPostgresBadDSN: pgx parses the DSN lazily, so an invalid port
// survives sql.Open and fails on the first migrate statement.
func TestOpenPostgresBadDSN(t *testing.T) {
	if _, err := Open("postgres://woca@127.0.0.1:notaport/woca"); err == nil {
		t.Fatal("malformed DSN should fail at migrate time")
	} else if !strings.Contains(err.Error(), "invalid port") {
		t.Fatalf("want invalid-port error, got %v", err)
	}
}

// TestOpenSQLiteBadQueryEscape: an invalid percent-escape in the DSN query
// (via a target that itself carries "?...%zz") is rejected when the first
// connection opens, surfacing as a migrate failure.
func TestOpenSQLiteBadQueryEscape(t *testing.T) {
	if _, err := Open("sqlite:" + filepath.Join(t.TempDir(), "q") + "?_pragma=%zz"); err == nil {
		t.Fatal("invalid percent-escape in DSN query should fail")
	} else if !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("want URL escape error, got %v", err)
	}
}

// TestSessionStatePostgresDialectPaths runs the postgres-flavored session
// state queries. A closed DB masquerading as postgres deterministically fails
// every Exec/Query, covering the error returns and the $N placeholder branch.
func TestSessionStatePostgresDialectPaths(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	pg := &DB{DB: db.DB, Dialect: DialectPostgres, Path: db.Path}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := pg.SetSessionState(SessionState{SessionID: "s1", Status: SessionRunning, RunID: "r1", Owner: "p@h", UpdatedAt: Now()}); err == nil {
		t.Fatal("SetSessionState on closed DB should fail")
	}
	if _, err := pg.ClearSessionState("s1", "r1"); err == nil {
		t.Fatal("ClearSessionState on closed DB should fail")
	}
	if err := pg.DeleteSessionState("s1"); err == nil {
		t.Fatal("DeleteSessionState on closed DB should fail")
	}
	if _, err := pg.SessionStates(); err == nil {
		t.Fatal("SessionStates on closed DB should fail")
	}
}

// TestSessionStatesScanError inserts a non-numeric updated_at (SQLite keeps
// it as TEXT under BIGINT affinity) so the scan into int64 fails.
func TestSessionStatesScanError(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO session_state (session_id, status, run_id, owner, updated_at)
		VALUES ('s', 'running', 'r', 'o', 'not-a-number')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionStates(); err == nil {
		t.Fatal("want scan error for TEXT updated_at")
	}
}

// TestRedactRefShapes covers the redaction helper's no-slash and
// single-slash shapes directly.
func TestRedactRefShapes(t *testing.T) {
	if got := redactRef("nocolon"); got != "nocolon" {
		t.Fatalf("no-slash ref = %q", got)
	}
	// The helper truncates at the second slash's offset (kept verbatim:
	// scheme, colon, and the slash pair collapse to the prefix up to it).
	if got := redactRef("sch://host/db"); got != "sch:/" {
		t.Fatalf("full ref = %q", got)
	}
	if got := redactRef("a/b"); got != "a/b" {
		t.Fatalf("single-slash ref = %q", got)
	}
}

// skipIfRootOrWindows guards permission-based tests: root ignores file modes
// and Windows ACLs behave differently.
func skipIfRootOrWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
}

// TestLoadOrCreateCryptoDirFailure covers the MkdirAll failure branch: the
// key path sits under a read-only directory, so the parent cannot be created.
func TestLoadOrCreateCryptoDirFailure(t *testing.T) {
	skipIfRootOrWindows(t)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateCrypto(filepath.Join(ro, "sub", "secret.key"))
	if err == nil {
		t.Fatal("want create-key-dir failure under read-only parent")
	}
	if !strings.Contains(err.Error(), "create key dir") {
		t.Fatalf("want create key dir error, got %v", err)
	}
}

// TestLoadOrCreateCryptoWriteFailure covers the temp-key write failure: the
// key directory exists but is read-only, so ReadFile misses (ENOENT) while
// MkdirAll succeeds and WriteFile fails.
func TestLoadOrCreateCryptoWriteFailure(t *testing.T) {
	skipIfRootOrWindows(t)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro2")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateCrypto(filepath.Join(ro, "secret.key"))
	if err == nil {
		t.Fatal("want write-key failure in read-only dir")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Fatalf("want key creation error, got %v", err)
	}
}
