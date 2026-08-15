package wocadb

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Crypto encrypts secrets at rest with AES-256-GCM. The master key is a
// 32-byte random value stored beside the database (default
// ~/.woca/secret.key, mode 0600) and created on first use.
type Crypto struct {
	aead cipher.AEAD
}

// KeyPath returns the default master-key location (~/.woca/secret.key).
func KeyPath() string { return filepath.Join(DefaultDir(), "secret.key") }

// LoadOrCreateCrypto loads the master key from keyPath, generating and
// persisting a new key (with parent directories) when the file is absent.
func LoadOrCreateCrypto(keyPath string) (*Crypto, error) {
	if keyPath == "" {
		keyPath = KeyPath()
	}
	key, err := os.ReadFile(keyPath)
	if err == nil {
		return newCrypto(key)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wocadb: read key: %w", err)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("wocadb: generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return nil, fmt.Errorf("wocadb: create key dir: %w", err)
	}
	// Write via temp file + rename so a partial key is never observed.
	tmp := keyPath + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, fmt.Errorf("wocadb: write key: %w", err)
	}
	if err := os.Rename(tmp, keyPath); err != nil {
		return nil, fmt.Errorf("wocadb: rename key: %w", err)
	}
	return newCrypto(key)
}

func newCrypto(key []byte) (*Crypto, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("wocadb: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Crypto{aead: aead}, nil
}

// Encrypt seals plaintext as nonce || ciphertext (base responsibility of the
// caller to store the raw bytes, e.g. in the secrets table).
func (c *Crypto) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt opens a nonce-prefixed ciphertext produced by Encrypt.
func (c *Crypto) Decrypt(data []byte) (string, error) {
	ns := c.aead.NonceSize()
	if len(data) < ns {
		return "", errors.New("wocadb: ciphertext too short")
	}
	plain, err := c.aead.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("wocadb: decrypt: %w", err)
	}
	return string(plain), nil
}
