package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestKeyUserIDEmptyHash(t *testing.T) {
	if got := (Key{}).UserID(); got != "" {
		t.Fatalf("zero-hash UserID = %q, want empty", got)
	}
}

func TestOpenKeyStoreErrors(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		if _, err := OpenKeyStore(""); err == nil {
			t.Fatal("empty path must fail")
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenKeyStore(filepath.Join(blocker, "relay.db")); err == nil {
			t.Fatal("database directory creation must fail")
		}
	})
	t.Run("path is a directory", func(t *testing.T) {
		// sql.Open accepts the DSN; the ping surfaces the failure.
		if _, err := OpenKeyStore(t.TempDir()); err == nil {
			t.Fatal("directory path must fail at ping")
		}
	})
	t.Run("unsupported schema version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.db")
		ks := openTestKeyStoreAt(t, path)
		if _, err := ks.db.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			currentSchemaVersion+1, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if err := ks.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenKeyStore(path); err == nil ||
			!strings.Contains(err.Error(), "unsupported schema version") {
			t.Fatalf("reopen at a newer schema: %v", err)
		}
	})
}

func openTestKeyStoreAt(t *testing.T, path string) *KeyStore {
	t.Helper()
	ks, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	return ks
}

func TestKeyStoreOpsOnClosedDB(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	if _, _, err := ks.CreateKey(ctx, "home-mac"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ks.CreateKey(ctx, "second"); err == nil {
		t.Fatal("create on a closed store must fail")
	}
	if _, err := ks.ListKeys(ctx); err == nil {
		t.Fatal("list on a closed store must fail")
	}
	if _, _, err := ks.RevokeKey(ctx, "home-mac"); err == nil {
		t.Fatal("revoke on a closed store must fail")
	}
	if _, err := ks.ListRevokedKeys(ctx); err == nil {
		t.Fatal("list-revoked on a closed store must fail")
	}
	if _, err := ks.KeyByID(ctx, 1); err == nil {
		t.Fatal("key-by-id on a closed store must fail")
	}
	if _, _, err := ks.CreateSession(ctx, 1); err == nil {
		t.Fatal("create-session on a closed store must fail")
	}
	if _, err := ks.ResolveSession(ctx, "tok"); err == nil || errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("resolve on a closed store: %v, want a store error", err)
	}
}

func TestKeyByID(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ks.KeyByID(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "home-mac" || got.ID != k.ID {
		t.Fatalf("KeyByID returned %+v", got)
	}
	if _, err := ks.KeyByID(ctx, 9999); err == nil {
		t.Fatal("unknown id must fail")
	}
}

func TestCreateKeyNameValidation(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	if _, _, err := ks.CreateKey(ctx, ""); err == nil {
		t.Fatal("empty name must fail")
	}
	if _, _, err := ks.CreateKey(ctx, strings.Repeat("n", 129)); err == nil {
		t.Fatal("over-long name must fail")
	}
}
