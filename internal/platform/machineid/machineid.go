// Package machineid implements the shared machine-identity skeleton for
// agents that register with a control plane (product design §2.3/§2.4):
// one-time enrollment tokens, locally generated Ed25519 identities,
// opaque short-TTL access tokens with lookup-table revocation, and
// signature challenges. The same skeleton serves bridges today and
// workers/executors tomorrow — that is why it lives in platform (design
// §2.4 推论), stdlib-only, with persistence behind the Registry interface.
//
// Threat model (per the nine-product survey): enrollment tokens are
// single-use, short-TTL, and stored server-side as hashes; the private
// key never leaves the device; access tokens are opaque and
// revocable-by-deletion, not self-asserting JWTs.
package machineid

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Sentinel errors for the registry surface.
var (
	ErrEnrollment = errors.New("machineid: enrollment token invalid, expired, or used")
	ErrUnknown    = errors.New("machineid: unknown access token")
	ErrRevoked    = errors.New("machineid: machine identity revoked")
)

// Token schemes: enrollment (one-time) and machine access (short-TTL).
const (
	EnrollScheme = "woca_enroll"
	AccessScheme = "wocm"
)

// NewEnrollmentToken mints a one-time enrollment token; the caller stores
// only HashToken's result. 32 bytes of crypto/rand.
func NewEnrollmentToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return EnrollScheme + "_" + hex.EncodeToString(raw), nil
}

// NewAccessToken mints an opaque machine access token.
func NewAccessToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return AccessScheme + "_" + hex.EncodeToString(raw), nil
}

// HashToken is the storage form of any token (SHA-256, hex).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AttachEnrollmentPin appends a k3s-style SPKI pin suffix to a minted
// token (design §2.3: the credential itself carries the expected edge
// identity, so a poisoned "enroll against my fake edge" attack fails
// before the one-time credential is spent).
func AttachEnrollmentPin(base, pin string) string {
	if pin == "" {
		return base
	}
	return base + "." + pin
}

// SplitEnrollment separates the pin suffix. The base is the credential
// (hash it for storage/lookup); the pin is carried information the bridge
// verifies the edge against. Legacy tokens have no suffix.
func SplitEnrollment(token string) (base, pin string) {
	if before, after, ok := strings.Cut(token, "."); ok {
		return before, after
	}
	return token, ""
}

// ValidEnrollment checks the token shape before any lookup, mirroring the
// tenant key discipline (reject is not a lookup).
func ValidEnrollment(token string) bool {
	base, pin := SplitEnrollment(token)
	if pin != "" && !isHex32(pin) {
		return false // a pin, when present, is a SHA-256 SPKI digest
	}
	parts := strings.Split(base, "_")
	if len(parts) != 3 || parts[0]+"_"+parts[1] != EnrollScheme {
		return false
	}
	return isHex32(parts[2]) // 32 bytes → 64 hex chars
}

// GenerateIdentity creates a fresh Ed25519 keypair; the private key stays
// on the device that generated it (Tailscale node-key discipline).
func GenerateIdentity() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// PublicKeyPEM encodes a public key for registration submissions.
func PublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(pub)
}

// ParsePublicKeyPEM decodes a registered public key.
func ParsePublicKeyPEM(b []byte) (ed25519.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("machineid: want ed25519 public key")
	}
	return ed, nil
}

// SPKIPin returns the SHA-256 fingerprint of a certificate's public-key
// info — the k3s-style pin enrollment tokens carry so the agent can
// verify the control plane before submitting its credential.
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// Challenge binds a message to the machine's key: the control plane sends
// a fresh nonce, the machine signs it, the registry-side verifies with
// the registered key (reserved hardening for token replay, §2.3).
type Challenge struct {
	Nonce     string `json:"nonce"`
	ExpiresAt int64  `json:"expires_at"`
}

func NewChallenge(ttl time.Duration) Challenge {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return Challenge{Nonce: hex.EncodeToString(raw), ExpiresAt: time.Now().Add(ttl).UnixMilli()}
}

func (c Challenge) Expired() bool { return time.Now().UnixMilli() > c.ExpiresAt }

// VerifyChallenge checks a signature over the nonce bytes.
func VerifyChallenge(pub ed25519.PublicKey, ch Challenge, sig []byte) bool {
	if ch.Expired() || ch.Nonce == "" || len(sig) == 0 {
		return false
	}
	return ed25519.Verify(pub, []byte(ch.Nonce), sig)
}

// Registry is the control-plane state of the machine-identity skeleton:
// enrollment consumption, bridge registration, access-token lifecycle.
// Implementations must be safe for concurrent use.
type Registry interface {
	// ConsumeEnrollment burns a one-time enrollment token and registers
	// the machine's public key, returning the new bridge id.
	ConsumeEnrollment(ctx context.Context, tokenHash string, pub []byte, ttl time.Duration) (bridgeID string, err error)
	// IssueAccess mints a short-TTL access token for a registered bridge.
	IssueAccess(ctx context.Context, bridgeID string, ttl time.Duration) (token string, expiresAt int64, err error)
	// ResolveAccess maps an access-token hash to its bridge; revoked
	// bridges and expired tokens are unknown.
	ResolveAccess(ctx context.Context, tokenHash string) (bridgeID string, err error)
	// RenewAccess exchanges a live token for a fresh one.
	RenewAccess(ctx context.Context, tokenHash string, ttl time.Duration) (token string, expiresAt int64, err error)
	// RevokeBridge deletes a bridge registration and cascades to its
	// tokens (long connections drop immediately; worst-case TTL leak
	// bound is the token lifetime).
	RevokeBridge(ctx context.Context, bridgeID string) error
}

// MemRegistry is an in-memory Registry for tests and ephemeral control
// planes. Access tokens default to the TTL passed at issue time.
type MemRegistry struct {
	mu      sync.Mutex
	enroll  map[string]memEnrollment
	bridges map[string]memBridge
	access  map[string]memAccess
	nextID  int
	nowFunc func() time.Time
}

type memEnrollment struct {
	expires time.Time
	used    bool
}

type memBridge struct {
	pub     []byte
	revoked bool
}

type memAccess struct {
	bridgeID string
	expires  time.Time
}

// NewMemRegistry builds the in-memory registry.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		enroll:  map[string]memEnrollment{},
		bridges: map[string]memBridge{},
		access:  map[string]memAccess{},
		nowFunc: time.Now,
	}
}

// AddEnrollment records a one-time enrollment token hash with a TTL.
func (m *MemRegistry) AddEnrollment(tokenHash string, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enroll[tokenHash] = memEnrollment{expires: m.nowFunc().Add(ttl)}
}

// ConsumeEnrollment implements Registry.
func (m *MemRegistry) ConsumeEnrollment(_ context.Context, tokenHash string, pub []byte, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enroll[tokenHash]
	if !ok || e.used || m.nowFunc().After(e.expires) {
		// Constant-shape failure: no oracle about which part failed.
		return "", ErrEnrollment
	}
	m.enroll[tokenHash] = memEnrollment{expires: e.expires, used: true}
	m.nextID++
	id := fmt.Sprintf("br_%08x", m.nextID)
	m.bridges[id] = memBridge{pub: append([]byte(nil), pub...)}
	return id, nil
}

// IssueAccess implements Registry.
func (m *MemRegistry) IssueAccess(_ context.Context, bridgeID string, ttl time.Duration) (string, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bridges[bridgeID]
	if !ok || b.revoked {
		return "", 0, ErrRevoked
	}
	token, err := NewAccessToken()
	if err != nil {
		return "", 0, err
	}
	m.access[HashToken(token)] = memAccess{bridgeID: bridgeID, expires: m.nowFunc().Add(ttl)}
	return token, m.nowFunc().Add(ttl).UnixMilli(), nil
}

// ResolveAccess implements Registry.
func (m *MemRegistry) ResolveAccess(_ context.Context, tokenHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.access[tokenHash]
	if !ok || m.nowFunc().After(a.expires) {
		return "", ErrUnknown
	}
	b, ok := m.bridges[a.bridgeID]
	if !ok || b.revoked {
		return "", ErrRevoked
	}
	return a.bridgeID, nil
}

// RenewAccess implements Registry.
func (m *MemRegistry) RenewAccess(ctx context.Context, tokenHash string, ttl time.Duration) (string, int64, error) {
	bridgeID, err := m.ResolveAccess(ctx, tokenHash)
	if err != nil {
		return "", 0, err
	}
	m.mu.Lock()
	delete(m.access, tokenHash)
	m.mu.Unlock()
	return m.IssueAccess(ctx, bridgeID, ttl)
}

// RevokeBridge implements Registry.
func (m *MemRegistry) RevokeBridge(_ context.Context, bridgeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.bridges[bridgeID]; ok && !b.revoked {
		m.bridges[bridgeID] = memBridge{pub: b.pub, revoked: true}
	}
	for h, a := range m.access {
		if a.bridgeID == bridgeID {
			delete(m.access, h)
		}
	}
	return nil
}

// ConstantTimeEqual compares two hex strings without early exit.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func isHex32(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
