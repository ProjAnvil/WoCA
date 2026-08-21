package dpop

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// signSegments signs an arbitrary two-segment signing input (header.payload)
// with key, returning a full compact proof.
func signSegments(t *testing.T, key testKey, headJSON, payloadSeg string) string {
	t.Helper()
	si := base64.RawURLEncoding.EncodeToString([]byte(headJSON)) + "." + payloadSeg
	sum := sha256.Sum256([]byte(si))
	sig, err := ecdsa.SignASN1(rand.Reader, key.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return si + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// realHeaderJSON returns the proof header JSON carrying key's public JWK.
func realHeaderJSON(t *testing.T, key testKey) string {
	t.Helper()
	head := proofHeader{
		Typ: proofTyp,
		Alg: proofAlg,
		JWK: jwk{
			Kty: "EC",
			Crv: "P-256",
			X:   base64.RawURLEncoding.EncodeToString(key.priv.PublicKey.X.Bytes()),
			Y:   base64.RawURLEncoding.EncodeToString(key.priv.PublicKey.Y.Bytes()),
		},
	}
	b, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func claimsSeg(t *testing.T, htm, htu, token string, at time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"htm": htm, "htu": htu, "iat": at.Unix(), "ath": accessTokenHash(token)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(claims)
}

func TestVerifyHeaderDecodeFailures(t *testing.T) {
	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	req := Request{Method: http.MethodGet, URI: "https://bff.example.com/api"}
	goodClaims := claimsSeg(t, http.MethodGet, "https://bff.example.com/api", "tok", now)

	cases := map[string]string{
		// parts[0] itself carries invalid base64url characters.
		"header not base64": "!!!." + goodClaims + ".AA",
		"header not json":   base64.RawURLEncoding.EncodeToString([]byte("not-json")) + "." + goodClaims + ".AA",
		"wrong typ": signSegments(t, key, `{"typ":"JWT","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}}`, goodClaims),
		"wrong alg": signSegments(t, key, `{"typ":"dpop+jwt","alg":"RS256","jwk":{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}}`, goodClaims),
		"x not base64": signSegments(t, key, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"!!","y":"AA"}}`, goodClaims),
		"y not base64": signSegments(t, key, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"AA","y":"!!"}}`, goodClaims),
		"point off curve": signSegments(t, key, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"AQ","y":"AQ"}}`, goodClaims),
	}
	for name, proof := range cases {
		if err := v.Verify(proof, req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
			t.Fatalf("%s: Verify = %v, want ErrInvalidProof", name, err)
		}
	}

	// Signature segment not base64: header is genuine so the failure is at
	// signature decoding, before any signature math.
	badSig := base64.RawURLEncoding.EncodeToString([]byte(realHeaderJSON(t, key))) + "." + goodClaims + ".!!"
	if err := v.Verify(badSig, req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("sig not base64: Verify = %v", err)
	}

	// Genuine header, garbage-but-valid-base64 signature: ECDSA verify fails.
	badSig2 := signSegments(t, key, realHeaderJSON(t, key), goodClaims)
	// re-sign is valid; corrupt only the signature bytes.
	corrupted := base64.RawURLEncoding.EncodeToString([]byte("corrupted-signature-bytes"))
	badSig2 = badSig2[:len(badSig2)-len(corrupted)] + corrupted
	if err := v.Verify(badSig2, req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("bad signature: Verify = %v", err)
	}
}

func TestVerifyClaimsDecodeFailures(t *testing.T) {
	key := newTestKey(t)
	now := time.Now()
	v := NewVerifier(WithClock(func() time.Time { return now }))
	req := Request{Method: http.MethodGet, URI: "https://bff.example.com/api"}
	head := realHeaderJSON(t, key)

	// Payload segment not base64 (signed over it, so signature passes).
	if err := v.Verify(signSegments(t, key, head, "!!"), req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("claims not base64: Verify = %v", err)
	}
	// Payload decodes but is not JSON.
	if err := v.Verify(signSegments(t, key, head, base64.RawURLEncoding.EncodeToString([]byte("not-json"))), req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("claims not json: Verify = %v", err)
	}
	// Fully valid proof except the jti claim is missing.
	if err := v.Verify(signSegments(t, key, head, claimsSeg(t, http.MethodGet, "https://bff.example.com/api", "tok", now)), req, "tok", key.pub); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("empty jti: Verify = %v", err)
	}
}

func TestReplayCacheEvictsOldest(t *testing.T) {
	c := newReplayCache(3, time.Hour)
	t0 := time.Now()
	if !c.add("a", t0) || !c.add("b", t0) || !c.add("c", t0) {
		t.Fatal("first three adds rejected")
	}
	// A resident jti within the TTL is rejected.
	if c.add("a", t0.Add(time.Second)) {
		t.Fatal("resident jti re-accepted")
	}
	// Capacity reached: adding d evicts the oldest entry (a).
	if !c.add("d", t0.Add(2*time.Second)) {
		t.Fatal("add d rejected")
	}
	// a was evicted, so it is accepted again despite being seen before.
	if !c.add("a", t0.Add(3*time.Second)) {
		t.Fatal("evicted jti not re-accepted")
	}
	// c is still resident.
	if c.add("c", t0.Add(4*time.Second)) {
		t.Fatal("resident c re-accepted")
	}
	// After the TTL a resident jti becomes acceptable again.
	if !c.add("c", t0.Add(2*time.Hour)) {
		t.Fatal("expired jti not re-accepted")
	}
}

func TestParseJWKThumbprintRejectsBadFieldTypes(t *testing.T) {
	// Unmarshals into map[string]json.RawMessage but not into the jwk struct.
	if _, err := ParseJWKThumbprint([]byte(`{"kty":123,"crv":"P-256","x":"AA","y":"AA"}`)); err == nil {
		t.Fatal("numeric kty accepted")
	}
}
