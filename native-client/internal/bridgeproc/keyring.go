package bridgeproc

// API-key storage for remote bridge profiles (product design §2.2): the
// OS credential store (Keychain / Credential Manager / libsecret) is the
// source of truth; a 0600 file under ~/.woca/native backs it up where no
// keyring exists (headless Linux, CI).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	gokeyring "github.com/zalando/go-keyring"
)

const keyringService = "woca-bridge"

// keyring calls are injectable so tests exercise the deterministic file
// fallback (the OS keychain needs a real user session and can block in
// sandboxes).
var (
	keyringSet   = gokeyring.Set
	keyringGet   = gokeyring.Get
	keyringErase = gokeyring.Delete
)

type keyFile struct {
	Keys map[string]string `json:"keys"`
}

// KeyStore stores bridge API keys per server URL.
type KeyStore struct {
	dir string

	mu   sync.Mutex
	file keyFile
}

// OpenKeyStore builds the store rooted at the native data dir.
func OpenKeyStore(dataDir string) (*KeyStore, error) {
	dir := filepath.Join(dataDir, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ks := &KeyStore{dir: dir, file: keyFile{Keys: map[string]string{}}}
	if data, err := os.ReadFile(ks.path()); err == nil {
		_ = json.Unmarshal(data, &ks.file)
	}
	return ks, nil
}

func (k *KeyStore) path() string { return filepath.Join(k.dir, "bridges.json") }

// Set stores the key for a server URL (keyring first, file fallback).
func (k *KeyStore) Set(serverURL, key string) error {
	k.mu.Lock()
	k.file.Keys[serverURL] = key
	k.mu.Unlock()
	if err := keyringSet(keyringService, serverURL, key); err == nil {
		return nil
	}
	data, _ := json.Marshal(k.file)
	return os.WriteFile(k.path(), data, 0o600)
}

// Get reads the key for a server URL ("" when absent).
func (k *KeyStore) Get(serverURL string) string {
	if v, err := keyringGet(keyringService, serverURL); err == nil && v != "" {
		return v
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.file.Keys[serverURL]
}

// Delete removes the stored key.
func (k *KeyStore) Delete(serverURL string) {
	_ = keyringErase(keyringService, serverURL)
	k.mu.Lock()
	delete(k.file.Keys, serverURL)
	k.mu.Unlock()
	data, _ := json.Marshal(k.file)
	_ = os.WriteFile(k.path(), data, 0o600)
}
