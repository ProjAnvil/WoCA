package configstore

import (
	"path/filepath"
	"testing"
)

func openSessionStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNativeSessionUpsertReadDelete(t *testing.T) {
	s := openSessionStore(t)
	ctx := t.Context()

	profile, err := s.SaveProfile(ctx, "https://enterprise.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.NativeSession(ctx, profile.ID); err != nil || ok {
		t.Fatalf("NativeSession on empty store = ok=%v err=%v", ok, err)
	}

	if err := s.SaveNativeSession(ctx, profile.ID, "device-1", []byte("cipher-1"), 1700000000); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.NativeSession(ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("NativeSession = ok=%v err=%v", ok, err)
	}
	if got.DeviceID != "device-1" || string(got.SessionCiphertext) != "cipher-1" || got.ExpiresAt != 1700000000 {
		t.Fatalf("session = %+v", got)
	}

	// Upsert replaces.
	if err := s.SaveNativeSession(ctx, profile.ID, "device-1", []byte("cipher-2"), 1800000000); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.NativeSession(ctx, profile.ID)
	if string(got.SessionCiphertext) != "cipher-2" || got.ExpiresAt != 1800000000 {
		t.Fatalf("upsert = %+v", got)
	}

	if err := s.DeleteNativeSession(ctx, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.NativeSession(ctx, profile.ID); ok {
		t.Fatal("session survived delete")
	}
}

func TestSaveNativeSessionRejectsUnknownProfile(t *testing.T) {
	s := openSessionStore(t)
	if err := s.SaveNativeSession(t.Context(), 999, "d", []byte("c"), 1); err == nil {
		t.Fatal("session saved for unknown profile")
	}
}

func TestTouchNativeSession(t *testing.T) {
	s := openSessionStore(t)
	ctx := t.Context()
	profile, err := s.SaveProfile(ctx, "https://enterprise.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNativeSession(ctx, profile.ID, "device-1", []byte("c"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchNativeSession(ctx, profile.ID, 42); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.NativeSession(ctx, profile.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.LastValidatedAt != 42 {
		t.Fatalf("LastValidatedAt = %d, want 42", got.LastValidatedAt)
	}
}
