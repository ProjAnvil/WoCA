// Package dpop implements the DPoP proof JWT (RFC 9449) subset the native
// client needs: ES256 proofs over P-256 with an embedded public JWK, the
// htm/htu/iat/jti/ath claims, RFC 7638 thumbprints, and a bounded replay
// cache. Signing is shared with the native-client module; verification is the
// gateway's job.
package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrInvalidProof reports a proof that failed verification. Callers must not
// unwrap details to clients: every failure is the same 401.
var ErrInvalidProof = errors.New("dpop: invalid proof")

const (
	proofTyp   = "dpop+jwt"
	proofAlg   = "ES256"
	proofSkew  = 30 * time.Second
	replayMax  = 8192
	replayTTL  = 5 * time.Minute
	athEntropy = sha256.Size
)

// Request is the server-side view of the request the proof must bind to.
type Request struct {
	Method string
	URI    string // normalized htu: no query, no fragment
}

// ProofInput carries the claims of a proof to sign (the native client side).
type ProofInput struct {
	HTM         string
	HTU         string
	AccessToken string
	IAT         time.Time
	JTI         string
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type proofHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	JWK jwk    `json:"jwk"`
}

type proofClaims struct {
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	JTI string `json:"jti"`
	ATH string `json:"ath,omitempty"`
}

// Sign produces a compact DPoP proof JWT for input, signed with priv.
func Sign(priv *ecdsa.PrivateKey, input ProofInput) (string, error) {
	if priv.Curve != elliptic.P256() {
		return "", fmt.Errorf("dpop: sign: key must be P-256")
	}
	header := proofHeader{
		Typ: proofTyp,
		Alg: proofAlg,
		JWK: jwk{
			Kty: "EC",
			Crv: "P-256",
			X:   base64.RawURLEncoding.EncodeToString(priv.PublicKey.X.Bytes()),
			Y:   base64.RawURLEncoding.EncodeToString(priv.PublicKey.Y.Bytes()),
		},
	}
	claims := proofClaims{
		HTM: input.HTM,
		HTU: input.HTU,
		IAT: input.IAT.Unix(),
		JTI: input.JTI,
	}
	if input.AccessToken != "" {
		claims.ATH = accessTokenHash(input.AccessToken)
	}
	head, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Thumbprint returns the RFC 7638 base64url SHA-256 thumbprint of a P-256
// public key: the canonical JSON of {"crv","kty","x","y"}.
func Thumbprint(pub ecdsa.PublicKey) (string, error) {
	if pub.Curve != elliptic.P256() {
		return "", fmt.Errorf("dpop: thumbprint: key must be P-256")
	}
	canonical := `{"crv":"P-256","kty":"EC","x":"` +
		base64.RawURLEncoding.EncodeToString(pub.X.Bytes()) +
		`","y":"` + base64.RawURLEncoding.EncodeToString(pub.Y.Bytes()) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// Verifier checks DPoP proofs. It is safe for concurrent use.
type Verifier struct {
	clock  func() time.Time
	replay *replayCache
}

type option func(*Verifier)

// WithClock overrides the verifier clock (tests).
func WithClock(clock func() time.Time) option {
	return func(v *Verifier) { v.clock = clock }
}

// NewVerifier returns a Verifier using the system clock.
func NewVerifier(opts ...option) *Verifier {
	v := &Verifier{clock: time.Now, replay: newReplayCache(replayMax, replayTTL)}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Verify validates a DPoP proof header against the request, the access token
// it must bind to, and the thumbprint pinned at activation. Every failure is
// ErrInvalidProof.
func (v *Verifier) Verify(proof string, req Request, accessToken, pinnedThumbprint string) error {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return ErrInvalidProof
	}
	var head proofHeader
	headJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalidProof
	}
	if err := json.Unmarshal(headJSON, &head); err != nil {
		return ErrInvalidProof
	}
	if head.Typ != proofTyp || head.Alg != proofAlg {
		return ErrInvalidProof
	}
	if head.JWK.Kty != "EC" || head.JWK.Crv != "P-256" || head.JWK.X == "" || head.JWK.Y == "" {
		return ErrInvalidProof
	}

	// The signature must verify under the embedded JWK.
	x, err := base64.RawURLEncoding.DecodeString(head.JWK.X)
	if err != nil {
		return ErrInvalidProof
	}
	y, err := base64.RawURLEncoding.DecodeString(head.JWK.Y)
	if err != nil {
		return ErrInvalidProof
	}
	xBig := newBig(x)
	yBig := newBig(y)
	if !elliptic.P256().IsOnCurve(xBig, yBig) {
		return ErrInvalidProof
	}
	pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: xBig, Y: yBig}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrInvalidProof
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.VerifyASN1(&pub, sum[:], sig) {
		return ErrInvalidProof
	}

	// The proof key must be the one pinned for this session.
	tp, err := thumbprintOfJWK(head.JWK)
	if err != nil || tp != pinnedThumbprint {
		return ErrInvalidProof
	}

	var claims proofClaims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrInvalidProof
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return ErrInvalidProof
	}
	if claims.HTM != req.Method || !equalHTU(claims.HTU, req.URI) {
		return ErrInvalidProof
	}
	if claims.ATH != accessTokenHash(accessToken) {
		return ErrInvalidProof
	}
	if claims.JTI == "" {
		return ErrInvalidProof
	}
	now := v.clock()
	if d := now.Sub(time.Unix(claims.IAT, 0)); d > proofSkew || d < -proofSkew {
		return ErrInvalidProof
	}
	if !v.replay.add(claims.JTI, now) {
		return ErrInvalidProof
	}
	return nil
}

func thumbprintOfJWK(k jwk) (string, error) {
	if k.Kty != "EC" || k.Crv != "P-256" || k.X == "" || k.Y == "" {
		return "", ErrInvalidProof
	}
	canonical := `{"crv":"` + k.Crv + `","kty":"` + k.Kty + `","x":"` + k.X + `","y":"` + k.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// accessTokenHash is RFC 9449 ath: base64url(SHA-256(access token ascii)).
func accessTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// equalHTU compares two htu values after normalization: same scheme/host
// (case-insensitive) and path, ignoring query and fragment.
func equalHTU(a, b string) bool {
	na, erra := normalizeHTU(a)
	nb, errb := normalizeHTU(b)
	if erra != nil || errb != nil {
		return false
	}
	return na == nb
}

func normalizeHTU(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("dpop: htu: %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	return scheme + "://" + host + u.EscapedPath(), nil
}

// replayCache is a bounded set of seen jti values with a TTL. Entries beyond
// capacity drop the oldest.
type replayCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	seen  map[string]time.Time
	order []string
}

func newReplayCache(max int, ttl time.Duration) *replayCache {
	return &replayCache{max: max, ttl: ttl, seen: make(map[string]time.Time, max)}
}

func (c *replayCache) add(jti string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.seen[jti]; ok && now.Sub(at) < c.ttl {
		return false
	}
	if len(c.order) >= c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, oldest)
	}
	c.seen[jti] = now
	c.order = append(c.order, jti)
	return true
}

func newBig(b []byte) *big.Int { return new(big.Int).SetBytes(b) }
