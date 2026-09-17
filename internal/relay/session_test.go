package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	token, expiresAt, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("empty session token")
	}
	if until := time.Until(expiresAt); until < SessionMaxAge-time.Minute || until > SessionMaxAge {
		t.Fatalf("expiresAt %v out, want ~30d", until)
	}
	sess, err := ks.ResolveSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.KeyID != k.ID || sess.KeyName != "sess" || sess.UserID != k.UserID() {
		t.Fatalf("session %+v (key %+v)", sess, k)
	}
}

func TestSessionSlidingAndAbsoluteExpiry(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "ttl")
	if err != nil {
		t.Fatal(err)
	}
	age := func(token string, created, touched time.Duration) {
		t.Helper()
		if _, err := ks.db.ExecContext(ctx,
			`UPDATE sessions SET created_at = ?, touched_at = ? WHERE token_hash = ?`,
			time.Now().UTC().Add(-created), time.Now().UTC().Add(-touched), HashToken(token)); err != nil {
			t.Fatal(err)
		}
	}

	// Idle beyond the 7d sliding window: dead even though 30d unspent.
	token, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token, 2*24*time.Hour, 8*24*time.Hour)
	if _, err := ks.ResolveSession(ctx, token); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("idle session: %v, want ErrSessionUnknown", err)
	}

	// Past the 30d absolute cap with a fresh touch: still dead (OWASP —
	// sliding expiry must have an absolute ceiling, design §4.4).
	token2, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token2, 31*24*time.Hour, time.Hour)
	if _, err := ks.ResolveSession(ctx, token2); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("absolute-expired session: %v, want ErrSessionUnknown", err)
	}

	// Inside both windows: alive, and resolving slides the touch forward.
	token3, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token3, 6*24*time.Hour, 6*24*time.Hour)
	sess, err := ks.ResolveSession(ctx, token3)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sess.TouchedAt) > time.Minute {
		t.Fatalf("sliding touch not refreshed: %v ago", time.Since(sess.TouchedAt))
	}
}

func TestSessionDeleteAndRevokeDelete(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "rev")
	if err != nil {
		t.Fatal(err)
	}

	// Logout is a server-side delete (design §4.4).
	token, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.DeleteSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.ResolveSession(ctx, token); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("deleted session: %v, want ErrSessionUnknown", err)
	}

	// DeleteSessionsForKey (maintenance/logout-everywhere API — NOT the
	// revocation path, which tombstones; Task 6) clears the whole key.
	t1, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.DeleteSessionsForKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{t1, t2} {
		if _, err := ks.ResolveSession(ctx, tok); !errors.Is(err, ErrSessionUnknown) {
			t.Fatalf("post-revoke session: %v, want ErrSessionUnknown", err)
		}
	}
}
