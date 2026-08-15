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
