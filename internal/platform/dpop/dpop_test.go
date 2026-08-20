package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"
)

type testKey struct {
	priv *ecdsa.PrivateKey
	pub  string // base64url JWK thumbprint of the public key
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{priv: priv, pub: tp}
}

func signProof(t *testing.T, key testKey, htm, htu, accessToken string, iat time.Time, jti string) string {
	t.Helper()
	proof, err := Sign(key.priv, ProofInput{HTM: htm, HTU: htu, AccessToken: accessToken, IAT: iat, JTI: jti})
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestVerifyAcceptsFreshProof(t *testing.T) {
	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))

	proof := signProof(t, key, http.MethodPost, "https://bff.example.com/api/models", "wsess_abc", now, "jti-1")
	if err := v.Verify(proof, Request{Method: http.MethodPost, URI: "https://bff.example.com/api/models"},
		"wsess_abc", key.pub); err != nil {
		t.Fatalf("Verify = %v", err)
	}
}

func TestVerifyRejectsEachTamperedDimension(t *testing.T) {
	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	const token = "wsess_abc"
	const uri = "https://bff.example.com/api/models"
	good := signProof(t, key, http.MethodGet, uri, token, now, "jti-ok")
	req := Request{Method: http.MethodGet, URI: uri}

	if err := v.Verify(good, req, token, key.pub); err != nil {
		t.Fatal(err)
	}

	// Wrong method.
	if err := v.Verify(good, Request{Method: http.MethodPost, URI: uri}, token, key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("htm mismatch = %v", err)
	}
	// Wrong URI.
	if err := v.Verify(good, Request{Method: http.MethodGet, URI: uri + "/x"}, token, key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("htu mismatch = %v", err)
	}
	// Wrong access token binding.
	if err := v.Verify(good, req, "wsess_other", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("ath mismatch = %v", err)
	}
	// Wrong pinned thumbprint (another device's key).
	other := newTestKey(t)
	if err := v.Verify(good, req, token, other.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("thumbprint mismatch = %v", err)
	}
	// Stale iat.
	stale := signProof(t, key, http.MethodGet, uri, token, now.Add(-2*time.Minute), "jti-stale")
	if err := v.Verify(stale, req, token, key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("stale iat = %v", err)
	}
	// Far-future iat.
	future := signProof(t, key, http.MethodGet, uri, token, now.Add(2*time.Minute), "jti-future")
	if err := v.Verify(future, req, token, key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("future iat = %v", err)
	}
	// Garbage tokens.
	for _, bad := range []string{"", "not-a-jwt", "aaa.bbb", "aaa.bbb.ccc.ddd"} {
		if err := v.Verify(bad, req, token, key.pub); !errors.Is(err, ErrInvalidProof) {
			t.Fatalf("Verify(%q) = %v", bad, err)
		}
	}
}

func TestVerifyReplayRejected(t *testing.T) {
	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	const token = "wsess_abc"
	const uri = "https://bff.example.com/api/models"
	proof := signProof(t, key, http.MethodGet, uri, token, now, "jti-replay")
	req := Request{Method: http.MethodGet, URI: uri}

	if err := v.Verify(proof, req, token, key.pub); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(proof, req, token, key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("replayed proof accepted: %v", err)
	}
	// A different jti is fine.
	fresh := signProof(t, key, http.MethodGet, uri, token, now, "jti-replay-2")
	if err := v.Verify(fresh, req, token, key.pub); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsProofSignedByDifferentKey(t *testing.T) {
	signer := newTestKey(t)
	pinner := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	proof, err := Sign(signer.priv, ProofInput{
		HTM: http.MethodGet, HTU: "https://bff.example.com/api",
		AccessToken: "wsess_abc", IAT: now, JTI: "jti-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The proof's embedded jwk is the signer's; the session pinned another
	// device's key, so possession of the pinned key is not proven.
	err = v.Verify(proof, Request{Method: http.MethodGet, URI: "https://bff.example.com/api"}, "wsess_abc", pinner.pub)
	if !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("cross-key proof = %v", err)
	}
}

func TestThumbprintIsStableAndBase64URL(t *testing.T) {
	key := newTestKey(t)
	tp, err := Thumbprint(key.priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if tp != key.pub {
		t.Fatalf("thumbprint not stable: %q vs %q", tp, key.pub)
	}
	if _, err := base64.RawURLEncoding.DecodeString(tp); err != nil {
		t.Fatalf("thumbprint is not base64url: %v", err)
	}
}
