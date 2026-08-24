package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

type notP256 struct{}

func TestNonP256KeysRejected(t *testing.T) {
	// P-384 key: every P-256-pinned entry point must reject it.
	priv384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(priv384, ProofInput{HTM: "GET", HTU: "https://x/y"}); err == nil {
		t.Fatal("Sign accepted P-384")
	}
	if _, err := Thumbprint(priv384.PublicKey); err == nil {
		t.Fatal("Thumbprint accepted P-384")
	}

	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	// A proof whose header advertises a bogus curve (signed correctly over
	// that header) is rejected by the JWK shape check.
	bad := signTamperedHeader(t, key, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-384","x":"AA","y":"AA"}}`,
		http.MethodGet, "https://bff.example.com/api", "tok", now)
	err = v.Verify(bad, Request{Method: http.MethodGet, URI: "https://bff.example.com/api"}, "tok", key.pub)
	if !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("bogus curve = %v", err)
	}
}

func signTamperedHeader(t *testing.T, key testKey, headerJSON, htm, htu, token string, at time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"htm": htm, "htu": htu, "iat": at.Unix(), "jti": "jti-t", "ath": accessTokenHash(token)})
	if err != nil {
		t.Fatal(err)
	}
	si := base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(si))
	sig, err := ecdsa.SignASN1(rand.Reader, key.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return si + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func mustSign(t *testing.T, key testKey, htm, htu, token string, at time.Time, jti string) string {
	t.Helper()
	p, err := Sign(key.priv, ProofInput{HTM: htm, HTU: htu, AccessToken: token, IAT: at, JTI: jti})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEqualHTUNormalization(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://X.COM/p", "https://x.com/p", true},
		{"https://x.com/p?q=1", "https://x.com/p", true}, // query stripped
		{"https://x.com/p#f", "https://x.com/p", true},   // fragment stripped
		{"http://x.com/p", "https://x.com/p", false},     // scheme mismatch
		{"https://x.com/a", "https://x.com/b", false},    // path mismatch
		{"not a url", "https://x.com/p", false},          // malformed
		{"https://x.com/p%20q", "https://x.com/p%20q", true},
	}
	for _, c := range cases {
		if got := equalHTU(c.a, c.b); got != c.want {
			t.Fatalf("equalHTU(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseJWKThumbprintRejectsMalformed(t *testing.T) {
	for _, bad := range []string{``, `not json`, `{"kty":"EC","crv":"P-256"}`, `{"d":"AA","kty":"EC","crv":"P-256","x":"AA","y":"AA"}`} {
		if _, err := ParseJWKThumbprint([]byte(bad)); err == nil {
			t.Fatalf("ParseJWKThumbprint(%q) accepted", bad)
		}
	}
	tp, err := ParseJWKThumbprint([]byte(`{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}`))
	if err != nil || tp == "" {
		t.Fatalf("public jwk = %q err=%v", tp, err)
	}
}
