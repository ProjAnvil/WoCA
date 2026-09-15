package bridge

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
)

func liveActivator(session string) *fakeActivator {
	return &fakeActivator{result: auth.ActivationResult{
		Ready: true, SessionID: session, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}}
}

func TestProfilesSortsLiveSessionsFirstAndReportsState(t *testing.T) {
	st := openVaultStore(t)
	vault := newVault(t, st)
	// A: live session. B: expired session. C: never signed in.
	if _, err := NewService(st, liveActivator("wsess_a"), WithSessionVault(vault)).Connect(t.Context(), ConnectionInput{
		ServerURL: "https://a.example.com", APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}
	profileB, err := st.SaveProfile(t.Context(), "https://b.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Save(t.Context(), profileB.ID, "wsess_b", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveProfile(t.Context(), "https://c.example.com"); err != nil {
		t.Fatal(err)
	}

	got, err := NewService(st, &fakeActivator{}, WithSessionVault(vault)).Profiles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Profiles() = %#v, want 3 rows", got)
	}
	// Live session first; the active flag tracks the last-saved profile (C).
	if got[0].ServerURL != "https://a.example.com" || got[0].SessionState != ProfileSessionActive {
		t.Fatalf("first row = %#v, want the live profile", got[0])
	}
	var activeURL string
	for _, row := range got {
		if row.Active {
			activeURL = row.ServerURL
		}
	}
	if activeURL != "https://c.example.com" {
		t.Fatalf("active profile = %q, want c", activeURL)
	}
	states := map[string]string{got[1].ServerURL: got[1].SessionState, got[2].ServerURL: got[2].SessionState}
	if states["https://b.example.com"] != ProfileSessionExpired || states["https://c.example.com"] != ProfileSessionSignedOut {
		t.Fatalf("states = %#v", states)
	}
}

func TestResumeProfileEntersAlongStoredSession(t *testing.T) {
	st := openVaultStore(t)
	vault := newVault(t, st)
	if _, err := NewService(st, liveActivator("wsess_a"), WithSessionVault(vault)).Connect(t.Context(), ConnectionInput{
		ServerURL: "https://a.example.com", APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}
	profileB, err := st.SaveProfile(t.Context(), "https://b.example.com")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, &fakeActivator{}, WithSessionVault(vault))

	// Without a stored session the resume activates the profile but lands
	// on the key form, URL prefilled.
	state, err := svc.ResumeProfile(t.Context(), profileB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded || state.ServerURL != "https://b.example.com" {
		t.Fatalf("ResumeProfile(no session) = %#v", state)
	}
	if active, ok, _ := st.ActiveProfile(t.Context()); !ok || active.BaseURL != "https://b.example.com" {
		t.Fatalf("ActiveProfile() = %#v ok=%v, want b", active, ok)
	}

	// Back to the profile with the live session: straight to ready.
	profileA, _, _ := st.ProfileByID(t.Context(), mustActiveID(t, st, "https://a.example.com"))
	state, err = svc.ResumeProfile(t.Context(), profileA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "https://a.example.com" {
		t.Fatalf("ResumeProfile(live) = %#v, want ready", state)
	}
}

func mustActiveID(t *testing.T, st *configstore.Store, baseURL string) int64 {
	t.Helper()
	rows, err := st.ListProfiles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.BaseURL == baseURL {
			return row.ID
		}
	}
	t.Fatalf("profile %s not found", baseURL)
	return 0
}

func TestResumeProfileRejectsUnknownID(t *testing.T) {
	svc := NewService(openTestStore(t), &fakeActivator{})
	if _, err := svc.ResumeProfile(t.Context(), 9999); err == nil {
		t.Fatal("ResumeProfile(unknown) error = nil")
	}
}

func TestDeleteProfileRevokesCascadesAndReports(t *testing.T) {
	st := openVaultStore(t)
	var revokePath atomic.Pointer[string]
	vault, bffURL := newRevokingVault(t, st, &revokePath)
	if err := st.SetProfileBridge(t.Context(), bffURL, "br_target"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(st, liveActivator("wsess_del"), WithSessionVault(vault)).Connect(t.Context(), ConnectionInput{
		ServerURL: bffURL, APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}
	id := mustActiveID(t, st, bffURL)
	svc := NewService(st, &fakeActivator{}, WithSessionVault(vault))

	out, err := svc.DeleteProfile(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted || !out.HadSession || !out.Revoked {
		t.Fatalf("DeleteProfile() = %#v, want deleted+revoked", out)
	}
	if p := revokePath.Load(); p == nil || *p != "/api/native/session" {
		t.Fatalf("revoke path = %v", p)
	}
	rows, err := st.ListProfiles(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListProfiles() after delete = %#v err=%v", rows, err)
	}
	if pin := st.ProfileBridge(t.Context(), bffURL); pin != "" {
		t.Fatalf("bridge pin survived delete: %q", pin)
	}
}

func TestDeleteProfileStillDeletesLocallyWhenRevocationFails(t *testing.T) {
	st := openVaultStore(t)
	material, err := devicekey.LoadOrCreate(t.TempDir() + "/device.key")
	if err != nil {
		t.Fatal(err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // revocation target unreachable — deletion must proceed
	vault := NewDeviceSessionVault(st, material, &http.Client{Timeout: 2 * time.Second})
	if _, err := NewService(st, liveActivator("wsess_dead"), WithSessionVault(vault)).Connect(t.Context(), ConnectionInput{
		ServerURL: deadURL, APIKey: "k",
	}); err != nil {
		t.Fatal(err)
	}
	id := mustActiveID(t, st, deadURL)
	svc := NewService(st, &fakeActivator{}, WithSessionVault(vault))

	out, err := svc.DeleteProfile(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted || !out.HadSession || out.Revoked {
		t.Fatalf("DeleteProfile() = %#v, want deleted without revocation", out)
	}
}
