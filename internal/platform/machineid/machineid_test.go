package machineid

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func pubOf(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey)
}

func signWith(priv ed25519.PrivateKey, ch Challenge) []byte {
	return ed25519.Sign(priv, []byte(ch.Nonce))
}

func TestEnrollmentTokenLifecycle(t *testing.T) {
	tok, err := NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidEnrollment(tok) {
		t.Fatalf("token %q rejected", tok)
	}
	if ValidEnrollment("woca_enroll_deadbeef") || ValidEnrollment("wocs_abc") {
		t.Fatal("malformed tokens accepted")
	}

	reg := NewMemRegistry()
	reg.AddEnrollment(HashToken(tok), time.Minute)

	priv, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, err := PublicKeyPEM(pubOf(priv))
	if err != nil {
		t.Fatal(err)
	}
	id, err := reg.ConsumeEnrollment(context.Background(), HashToken(tok), pubPEM, time.Hour)
	if err != nil || id == "" {
		t.Fatalf("consume = %q %v", id, err)
	}
	// One-time: second consumption fails.
	if _, err := reg.ConsumeEnrollment(context.Background(), HashToken(tok), pubPEM, time.Hour); err == nil {
		t.Fatal("enrollment token reused")
	}
	// Unknown token.
	if _, err := reg.ConsumeEnrollment(context.Background(), HashToken("woca_enroll_"+strings.Repeat("0", 64)), pubPEM, time.Hour); err == nil {
		t.Fatal("unknown enrollment accepted")
	}
}

func TestAccessTokensAndRevocation(t *testing.T) {
	reg := NewMemRegistry()
	priv, _ := GenerateIdentity()
	pubPEM, _ := PublicKeyPEM(pubOf(priv))
	tok, _ := NewEnrollmentToken()
	reg.AddEnrollment(HashToken(tok), time.Minute)
	id, err := reg.ConsumeEnrollment(context.Background(), HashToken(tok), pubPEM, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	access, exp, err := reg.IssueAccess(context.Background(), id, time.Hour)
	if err != nil || access == "" || exp <= time.Now().UnixMilli() {
		t.Fatalf("issue = %q %d %v", access, exp, err)
	}
	got, err := reg.ResolveAccess(context.Background(), HashToken(access))
	if err != nil || got != id {
		t.Fatalf("resolve = %q %v", got, err)
	}

	// Renewal rotates the token: the old one dies.
	fresh, _, err := reg.RenewAccess(context.Background(), HashToken(access), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ResolveAccess(context.Background(), HashToken(access)); err == nil {
		t.Fatal("old access token survived renewal")
	}
	if got, err := reg.ResolveAccess(context.Background(), HashToken(fresh)); err != nil || got != id {
		t.Fatalf("renewed resolve = %q %v", got, err)
	}

	// Revocation cascades instantly.
	if err := reg.RevokeBridge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ResolveAccess(context.Background(), HashToken(fresh)); err == nil {
		t.Fatal("revoked bridge still resolves")
	}
	if _, _, err := reg.IssueAccess(context.Background(), id, time.Hour); err == nil {
		t.Fatal("revoked bridge issued a token")
	}
}

func TestExpiredEnrollmentRejected(t *testing.T) {
	reg := NewMemRegistry()
	tok, _ := NewEnrollmentToken()
	reg.AddEnrollment(HashToken(tok), -time.Second) // already expired
	priv, _ := GenerateIdentity()
	pubPEM, _ := PublicKeyPEM(pubOf(priv))
	if _, err := reg.ConsumeEnrollment(context.Background(), HashToken(tok), pubPEM, time.Hour); err == nil {
		t.Fatal("expired enrollment accepted")
	}
}

func TestChallengeSignVerify(t *testing.T) {
	priv, _ := GenerateIdentity()
	ch := NewChallenge(time.Minute)
	sig := signWith(priv, ch)
	if !VerifyChallenge(pubOf(priv), ch, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyChallenge(pubOf(priv), NewChallenge(time.Minute), sig) {
		t.Fatal("wrong nonce accepted")
	}
	expired := NewChallenge(-time.Second)
	if VerifyChallenge(pubOf(priv), expired, signWith(priv, expired)) {
		t.Fatal("expired challenge accepted")
	}
}

func TestPublicKeyPEMRoundTrip(t *testing.T) {
	priv, _ := GenerateIdentity()
	pubPEM, err := PublicKeyPEM(pubOf(priv))
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParsePublicKeyPEM(pubPEM)
	if err != nil || !back.Equal(pubOf(priv)) {
		t.Fatalf("round trip = %v %v", back, err)
	}
	if _, err := ParsePublicKeyPEM([]byte("junk")); err == nil {
		t.Fatal("junk PEM accepted")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if ConstantTimeEqual("aa", "aa") != true || ConstantTimeEqual("aa", "ab") {
		t.Fatal("constant-time compare misbehaves")
	}
}

var _ ed25519.PublicKey // pin the import
