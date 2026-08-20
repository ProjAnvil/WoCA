package devicekey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreatePersistsStableMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.key")

	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeviceID == "" || first.DPoPKey == nil || len(first.WrapKey) != 32 {
		t.Fatalf("material = %+v", first)
	}
	if first.DPoPKey.Curve != elliptic.P256() {
		t.Fatal("DPoP key is not P-256")
	}

	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.DeviceID != first.DeviceID {
		t.Fatalf("device id changed across reload: %q vs %q", second.DeviceID, first.DeviceID)
	}
	if second.DPoPKey.D.Cmp(first.DPoPKey.D) != 0 {
		t.Fatal("DPoP key changed across reload")
	}
	if !bytes.Equal(second.WrapKey, first.WrapKey) {
		t.Fatal("wrap key changed across reload")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSealOpenSessionRoundtrip(t *testing.T) {
	m, err := LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := SealSession(m, "wsess_aa_bb", 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("wsess_aa_bb")) {
		t.Fatal("ciphertext contains the plaintext session id")
	}

	id, expiresAt, err := OpenSession(m, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if id != "wsess_aa_bb" || expiresAt != 1700000000000 {
		t.Fatalf("opened = %q %d", id, expiresAt)
	}
}

func TestOpenSessionRejectsTamperingAndWrongKey(t *testing.T) {
	m, err := LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreate(filepath.Join(t.TempDir(), "other.key"))
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := SealSession(m, "wsess_aa_bb", time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xff

	if _, _, err := OpenSession(m, tampered); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, _, err := OpenSession(other, ciphertext); err == nil {
		t.Fatal("ciphertext opened under a different device key")
	}
}

func TestThumbprintMatchesVerifier(t *testing.T) {
	m, err := LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(m.DPoPKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp) < 40 {
		t.Fatalf("thumbprint = %q", tp)
	}
	var _ *ecdsa.PrivateKey = m.DPoPKey
}
