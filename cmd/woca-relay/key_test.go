package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/relay"
)

// captureStdout swaps os.Stdout for a pipe while fn runs (the key CLI
// prints the raw key to real stdout exactly once, design §4.3).
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fnErr := fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), fnErr
}

func TestKeyCreateListRevoke(t *testing.T) {
	db := tempDB(t)

	out, err := captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "create", "home-mac")
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(out)
	if !strings.HasPrefix(raw, relay.KeyPrefix) {
		t.Fatalf("create did not print a %s key: %q", relay.KeyPrefix, out)
	}

	out, err = captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "list")
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "home-mac", raw[:8], "active", "offline"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
	// The full key is never listed (hash-only storage, design §4.1).
	if strings.Contains(out, raw) {
		t.Fatal("list leaked the full key")
	}

	out, err = captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "revoke", "home-mac")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `revoked key "home-mac"`) {
		t.Fatalf("revoke output: %q", out)
	}

	out, err = captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "list")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "revoked") {
		t.Fatalf("revoked status missing:\n%s", out)
	}
	// Revoking twice is an operator-visible error, not a silent no-op.
	if err := execRelayCmd(t, "--db", db, "key", "revoke", "home-mac"); err == nil {
		t.Fatal("second revoke must fail")
	}
	if err := execRelayCmd(t, "--db", db, "key", "revoke", "nope"); err == nil {
		t.Fatal("unknown-key revoke must fail")
	}
}

func TestKeyCreateDuplicateName(t *testing.T) {
	db := tempDB(t)
	if err := execRelayCmd(t, "--db", db, "key", "create", "dup"); err != nil {
		t.Fatal(err)
	}
	if err := execRelayCmd(t, "--db", db, "key", "create", "dup"); err == nil {
		t.Fatal("duplicate name accepted (name is UNIQUE)")
	}
}

func TestKeyArgValidation(t *testing.T) {
	db := tempDB(t)
	for _, args := range [][]string{
		{"key", "create"},           // missing name
		{"key", "create", "a", "b"}, // extra arg
		{"key", "revoke"},           // missing name
		{"key", "list", "extra"},    // list takes no args
	} {
		if err := execRelayCmd(t, append([]string{"--db", db}, args...)...); err == nil {
			t.Fatalf("args %v must fail", args)
		}
	}
}

func TestKeyCmdsUnopenableDB(t *testing.T) {
	// Every key subcommand surfaces the open error (no panics).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(blocker, "relay.db")
	for _, args := range [][]string{
		{"key", "create", "home-mac"},
		{"key", "revoke", "home-mac"},
	} {
		if err := execRelayCmd(t, append([]string{"--db", db}, args...)...); err == nil {
			t.Fatalf("%v with unopenable db must fail", args)
		}
	}
}

func TestKeyListBridgeOnline(t *testing.T) {
	db := tempDB(t)
	if _, err := captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "create", "home-mac")
	}); err != nil {
		t.Fatal(err)
	}
	// Tunnel activity inside the generation lease window marks the bridge
	// online (design §6.2) — stamp last_used_at directly.
	ks, err := relay.OpenKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	// A second key stays untouched (its last-used cell renders "-").
	if _, _, err := ks.CreateKey(t.Context(), "other"); err != nil {
		t.Fatal(err)
	}
	keys, err := ks.ListKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, kk := range keys {
		if kk.Name == "home-mac" {
			ks.TouchKey(t.Context(), kk.ID)
		}
	}
	if err := ks.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return execRelayCmd(t, "--db", db, "key", "list")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "online") {
		t.Fatalf("touched key not shown online:\n%s", out)
	}
	// The untouched key keeps the "-" last-used cell.
	if !strings.Contains(out, "-") {
		t.Fatalf("untouched key missing the dash cell:\n%s", out)
	}
}
