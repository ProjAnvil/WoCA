package wocadb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCryptoRoundTrip(t *testing.T) {
	c, err := LoadOrCreateCrypto(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := c.Encrypt("sk-ant-secret-123")
	if err != nil {
		t.Fatal(err)
	}
	if string(ct) == "sk-ant-secret-123" {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := c.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if pt != "sk-ant-secret-123" {
		t.Fatalf("plaintext = %q", pt)
	}
}

func TestCryptoDistinctCiphertexts(t *testing.T) {
	c, _ := LoadOrCreateCrypto(filepath.Join(t.TempDir(), "k"))
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if string(a) == string(b) {
		t.Fatal("nonce reuse: identical ciphertexts for identical plaintext")
	}
}

func TestCryptoWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	c1, _ := LoadOrCreateCrypto(filepath.Join(dir, "k1"))
	c2, _ := LoadOrCreateCrypto(filepath.Join(dir, "k2"))
	ct, _ := c1.Encrypt("secret")
	if _, err := c2.Decrypt(ct); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestCryptoKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if _, err := LoadOrCreateCrypto(key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, want 0600", info.Mode().Perm())
	}
	// Second load reuses the same key.
	c2, err := LoadOrCreateCrypto(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := c2.Encrypt("x")
	if _, err := c2.Decrypt(ct); err != nil {
		t.Fatal(err)
	}
}

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

func writeBlocker(path string) error {
	return os.WriteFile(path, []byte("x"), 0o644)
}
