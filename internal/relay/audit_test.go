package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAuditLogBadPath(t *testing.T) {
	// The audit target's parent is a regular file: open must fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditLog(filepath.Join(blocker, "audit.log")); err == nil {
		t.Fatal("audit log inside a file must fail")
	}
}

func TestAuditLogDiscardClose(t *testing.T) {
	// The empty path discards entries and closes nil-safe.
	a, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	a.Event(AuditBridgeUp, "home-mac", "")
	if err := a.Close(); err != nil {
		t.Fatalf("discard close: %v", err)
	}
}

func TestAuditLogEventFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Event(AuditRevocation, "home-mac", "key revoked")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	for _, want := range []string{`"event":"revocation"`, `"key":"home-mac"`, `"detail":"key revoked"`, `"ts":"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit line %s missing %s", line, want)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log mode %v, want 0600", info.Mode().Perm())
	}
}
