package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/native-client/internal/devicekey"
)

func TestDPoPCredentialsBindProofToDestination(t *testing.T) {
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := devicekey.Thumbprint(material.DPoPKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p := NewDPoPCredentials(material, func() (string, bool) { return "wsess_session", true })

	dest, _ := url.Parse("https://bff.example.com/api/models?x=1")
	creds, err := p.Credentials(context.Background(), "GET", dest)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Scheme != "DPoP" || creds.Session != "wsess_session" || creds.Proof == "" {
		t.Fatalf("credentials = %+v", creds)
	}
	// The proof must verify under the pinned thumbprint and bind to the
	// destination without its query string.
	v := dpop.NewVerifier()
	err = v.Verify(creds.Proof, dpop.Request{Method: "GET", URI: "https://bff.example.com/api/models"},
		"wsess_session", thumbprint)
	if err != nil {
		t.Fatalf("proof does not verify: %v", err)
	}
	if strings.Contains(creds.Proof, "x=1") {
		t.Fatal("proof binds to the query string")
	}
}

func TestDPoPCredentialsRequireStoredSession(t *testing.T) {
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewDPoPCredentials(material, func() (string, bool) { return "", false })
	dest, _ := url.Parse("https://bff.example.com/api/models")
	if _, err := p.Credentials(context.Background(), "GET", dest); err == nil {
		t.Fatal("credentials minted without a stored session")
	}
}

func TestDPoPProofsAreUniquePerRequest(t *testing.T) {
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewDPoPCredentials(material, func() (string, bool) { return "wsess_session", true })
	dest, _ := url.Parse("https://bff.example.com/api/models")
	first, err := p.Credentials(context.Background(), "GET", dest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Credentials(context.Background(), "GET", dest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Proof == second.Proof {
		t.Fatal("identical proofs minted for two requests (jti reuse)")
	}
	var _ *ecdsa.PrivateKey = material.DPoPKey
	_ = elliptic.P256
	_ = rand.Reader
	_ = time.Now
}
