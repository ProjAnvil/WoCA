// Package devicekey holds the per-device cryptographic material of the
// native client: a P-256 DPoP signing key (its thumbprint is pinned at
// activation), a 32-byte AES-256 key that wraps the opaque session id for
// SQLite storage, and a stable device id. v1 keeps the material in a single
// 0600 file under the app-data directory; the OS keychain remains the later
// secure-storage milestone.
package devicekey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// Material is the device's long-lived cryptographic identity.
type Material struct {
	DeviceID string
	DPoPKey  *ecdsa.PrivateKey
	WrapKey  []byte
}

type storedMaterial struct {
	DeviceID string `json:"device_id"`
	DPoPD    string `json:"dpop_d"`   // base64url private scalar
	WrapKey  string `json:"wrap_key"` // base64url 32 bytes
}

// LoadOrCreate returns the device material at path, generating and
// persisting it (mode 0600) on first use.
func LoadOrCreate(path string) (Material, error) {
	if b, err := os.ReadFile(path); err == nil {
		return parseMaterial(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Material{}, fmt.Errorf("devicekey: read: %w", err)
	}

	deviceID := randomToken(16)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, fmt.Errorf("devicekey: generate: %w", err)
	}
	wrap := make([]byte, 32)
	if _, err := rand.Read(wrap); err != nil {
		return Material{}, fmt.Errorf("devicekey: entropy: %w", err)
	}
	m := Material{DeviceID: deviceID, DPoPKey: priv, WrapKey: wrap}
	b, err := encodeMaterial(m)
	if err != nil {
		return Material{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Material{}, fmt.Errorf("devicekey: mkdir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return Material{}, fmt.Errorf("devicekey: write: %w", err)
	}
	return m, nil
}

func encodeMaterial(m Material) ([]byte, error) {
	out := storedMaterial{
		DeviceID: m.DeviceID,
		DPoPD:    base64.RawURLEncoding.EncodeToString(m.DPoPKey.D.Bytes()),
		WrapKey:  base64.RawURLEncoding.EncodeToString(m.WrapKey),
	}
	return json.Marshal(out)
}

func parseMaterial(b []byte) (Material, error) {
	var s storedMaterial
	if err := json.Unmarshal(b, &s); err != nil {
		return Material{}, fmt.Errorf("devicekey: parse: %w", err)
	}
	d, err := base64.RawURLEncoding.DecodeString(s.DPoPD)
	if err != nil || len(d) == 0 {
		return Material{}, fmt.Errorf("devicekey: parse: bad dpop scalar")
	}
	wrap, err := base64.RawURLEncoding.DecodeString(s.WrapKey)
	if err != nil || len(wrap) != 32 {
		return Material{}, fmt.Errorf("devicekey: parse: bad wrap key")
	}
	priv := new(ecdsa.PrivateKey)
	priv.Curve = elliptic.P256()
	priv.D = new(big.Int).SetBytes(d)
	priv.X, priv.Y = elliptic.P256().ScalarBaseMult(d)
	return Material{DeviceID: s.DeviceID, DPoPKey: priv, WrapKey: wrap}, nil
}

type sealedSession struct {
	Nonce      string `json:"n"`
	Ciphertext string `json:"c"`
	ExpiresAt  int64  `json:"e"`
}

// SealSession encrypts the opaque session id under the device wrap key
// (AES-256-GCM). The expiry travels inside the sealed envelope.
func SealSession(m Material, sessionID string, expiresAt int64) ([]byte, error) {
	block, err := aes.NewCipher(m.WrapKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, []byte(sessionID), nil)
	out, err := json.Marshal(sealedSession{
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ct),
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// OpenSession decrypts a sealed session id. Any tampering or key mismatch is
// an error.
func OpenSession(m Material, sealed []byte) (string, int64, error) {
	var s sealedSession
	if err := json.Unmarshal(sealed, &s); err != nil {
		return "", 0, fmt.Errorf("devicekey: open: %w", err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(s.Nonce)
	if err != nil {
		return "", 0, fmt.Errorf("devicekey: open: nonce")
	}
	ct, err := base64.RawURLEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return "", 0, fmt.Errorf("devicekey: open: ciphertext")
	}
	block, err := aes.NewCipher(m.WrapKey)
	if err != nil {
		return "", 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", 0, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", 0, fmt.Errorf("devicekey: open: %w", err)
	}
	return string(pt), s.ExpiresAt, nil
}

// Thumbprint returns the RFC 7638 base64url thumbprint of a P-256 public
// key (the value pinned at activation).
func Thumbprint(pub ecdsa.PublicKey) (string, error) {
	if pub.Curve != elliptic.P256() {
		return "", errors.New("devicekey: thumbprint: key must be P-256")
	}
	canonical := `{"crv":"P-256","kty":"EC","x":"` +
		base64.RawURLEncoding.EncodeToString(pub.X.Bytes()) +
		`","y":"` + base64.RawURLEncoding.EncodeToString(pub.Y.Bytes()) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// PublicJWK renders the public activation JWK.
func PublicJWK(pub ecdsa.PublicKey) ([]byte, error) {
	if pub.Curve != elliptic.P256() {
		return nil, errors.New("devicekey: jwk: key must be P-256")
	}
	return json.Marshal(map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(pub.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(pub.Y.Bytes()),
	})
}

func randomToken(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic("devicekey: entropy: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
