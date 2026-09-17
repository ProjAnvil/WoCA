package configstore

import (
	"path/filepath"
	"strings"
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

func TestSetProfileRelayTokenValidation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	if err := s.SetProfileRelayToken(ctx, "://bad-url", "sess-1"); err == nil {
		t.Fatal("invalid URL accepted")
	}
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", ""); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", strings.Repeat("t", 513)); err == nil {
		t.Fatal("over-long token accepted")
	}
	// A boundary-length token still fits.
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", strings.Repeat("t", 512)); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRelayTokenEdges(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	// An unnormalizable URL reads as unset instead of erroring.
	if got := s.ProfileRelayToken(ctx, "://bad-url"); got != "" {
		t.Fatalf("invalid URL token: %q", got)
	}
	// An over-long row (written outside the setter's validation) is
	// treated as corrupt and reads as unset.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)`,
		profileRelayTokenKey("https://relay.example.com"), strings.Repeat("t", 513), 1); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("over-long token read: %d bytes", len(got))
	}
}

func TestDeleteProfileRelayTokenInvalidURL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.DeleteProfileRelayToken(t.Context(), "://bad-url"); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestProfileRelayTokenClosedStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Closed DB: the setter and delete surface the driver error; the
	// getter reads as unset (best-effort lookup).
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-2"); err == nil {
		t.Fatal("set on closed store succeeded")
	}
	if err := s.DeleteProfileRelayToken(ctx, "https://relay.example.com"); err == nil {
		t.Fatal("delete on closed store succeeded")
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("closed-store read: %q", got)
	}
}
