package relay

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestKeyStore(t *testing.T) *KeyStore {
	t.Helper()
	ks, err := OpenKeyStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	return ks
}

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, KeyPrefix) {
		t.Fatalf("key %q lacks the %s prefix", a, KeyPrefix)
	}
	// wcr_ + 43 base64url chars (32 bytes, raw — no padding).
	if len(a) != len(KeyPrefix)+43 {
		t.Fatalf("key length %d, want %d", len(a), len(KeyPrefix)+43)
	}
	b, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints collided")
	}
}

func TestKeyStoreCreateLookupRevoke(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()

	k, raw, err := ks.CreateKey(ctx, "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	if k.Prefix != raw[:8] {
		t.Fatalf("prefix %q, want the key's first 8 chars", k.Prefix)
	}
	got, err := ks.LookupKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != k.ID || got.Name != "home-mac" {
		t.Fatalf("lookup returned %+v", got)
	}
	// user_id derives from the stored hash, never from the raw key.
	if got.UserID() == "" || strings.HasPrefix(raw, got.UserID()) {
		t.Fatalf("user id %q must be hash-derived", got.UserID())
	}
	if _, err := ks.LookupKey(ctx, "wcr_wrong"); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("wrong key: %v, want ErrKeyUnknown", err)
	}
	if _, err := ks.LookupKey(ctx, "not-a-key"); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("non-wcr token: %v, want ErrKeyUnknown", err)
	}
	if _, _, err := ks.CreateKey(ctx, "home-mac"); err == nil {
		t.Fatal("duplicate name accepted (name is UNIQUE)")
	}

	revoked, ok, err := ks.RevokeKey(ctx, "home-mac")
	if err != nil || !ok {
		t.Fatalf("revoke: err=%v ok=%v", err, ok)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked row lacks revoked_at")
	}
	if _, err := ks.LookupKey(ctx, raw); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("revoked lookup: %v, want ErrKeyRevoked", err)
	}
	// Idempotent: unknown or already-revoked returns ok=false, no error.
	if _, ok, err := ks.RevokeKey(ctx, "home-mac"); err != nil || ok {
		t.Fatalf("second revoke: err=%v ok=%v, want ok=false", err, ok)
	}
	if _, ok, err := ks.RevokeKey(ctx, "nope"); err != nil || ok {
		t.Fatalf("unknown revoke: err=%v ok=%v, want ok=false", err, ok)
	}

	if _, _, err := ks.CreateKey(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	list, err := ks.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len %d, want 2", len(list))
	}
}

func TestKeyStoreReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	ks, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := ks.CreateKey(context.Background(), "persist")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Close(); err != nil {
		t.Fatal(err)
	}
	ks2, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ks2.Close() }()
	if _, err := ks2.LookupKey(context.Background(), raw); err != nil {
		t.Fatalf("key lost across reopen: %v", err)
	}
}
