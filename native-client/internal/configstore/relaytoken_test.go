package configstore

import (
	"path/filepath"
	"testing"
)

func TestProfileRelayTokenRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("unset token: %q", got)
	}
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "sess-1" {
		t.Fatalf("token: %q", got)
	}
	// Re-login rotates the token in place.
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-2"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "sess-2" {
		t.Fatalf("rotated token: %q", got)
	}
	// A different profile has its own row.
	if got := s.ProfileRelayToken(ctx, "https://other.example.com"); got != "" {
		t.Fatalf("cross-profile leakage: %q", got)
	}
	// Profile deletion takes the token with it (mirrors the bridge pin).
	p, err := s.SaveProfile(ctx, "https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteProfile(ctx, p.ID); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("token survived profile deletion: %q", got)
	}
}

func TestDeleteProfileRelayToken(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-1"); err != nil {
		t.Fatal(err)
	}
	// Deleting an unset row is a no-op, not an error (logout is
	// idempotent for direct-bridge profiles).
	if err := s.DeleteProfileRelayToken(ctx, "https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfileRelayToken(ctx, "https://relay.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("token survived delete: %q", got)
	}
}
