package devicekey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
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
	jwkJSON, err := PublicJWK(m.DPoPKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// The verifier side (server) hashes the client's JWK string fields; the
	// pinned thumbprint must equal what that side computes.
	vtp, err := dpop.ParseJWKThumbprint(jwkJSON)
	if err != nil {
		t.Fatal(err)
	}
	if tp != vtp {
		t.Fatalf("thumbprint %q != verifier thumbprint %q", tp, vtp)
	}
	var _ *ecdsa.PrivateKey = m.DPoPKey
}

// P-256 coordinates must be fixed-width 32 octets (RFC 7518 §6.2.1.2): a
// coordinate with a leading zero byte must keep it, or the JWK — and its
// RFC 7638 thumbprint — diverges from any compliant verifier.
func TestJWKCoordPadsTo32Octets(t *testing.T) {
	short := big.NewInt(1) // worst case: 31 leading zero octets
	got := jwkCoord(short)
	raw, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 {
		t.Fatalf("jwkCoord(1) = %d octets, want 32", len(raw))
	}
	if raw[31] != 1 || !bytes.Equal(raw[:31], make([]byte, 31)) {
		t.Fatalf("jwkCoord(1) = %x, want 31 zero octets then 0x01", raw)
	}

	// A key whose X coordinate genuinely starts with 0x00 must still
	// thumbprint consistently between Thumbprint and the verifier path.
	for i := 0; i < 2000; i++ {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(key.X.Bytes()) < 32 || len(key.Y.Bytes()) < 32 {
			tp, err := Thumbprint(key.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			jwkJSON, err := PublicJWK(key.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			vtp, err := dpop.ParseJWKThumbprint(jwkJSON)
			if err != nil {
				t.Fatal(err)
			}
			if tp != vtp {
				t.Fatalf("short-coordinate key: thumbprint %q != verifier %q", tp, vtp)
			}
			return
		}
	}
	t.Fatal("no short-coordinate key found in 2000 tries (want ~1% hit rate)")
}
