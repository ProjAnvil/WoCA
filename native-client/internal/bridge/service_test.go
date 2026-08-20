package bridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/configstore"
)

type recordingActivator struct {
	request auth.ActivationRequest
	result  auth.ActivationResult
	err     error
}

func (a *recordingActivator) Activate(_ context.Context, request auth.ActivationRequest) (auth.ActivationResult, error) {
	a.request = request
	return a.result, a.err
}

func TestBootstrapIsUnconfiguredWithoutProfile(t *testing.T) {
	store := openTestStore(t)
	service := NewService(store, &recordingActivator{})

	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateUnconfigured || state.ServerURL != "" {
		t.Fatalf("Bootstrap() = %#v, want unconfigured without URL", state)
	}
}

func TestConnectPersistsURLButNeverAPIKey(t *testing.T) {
	const apiKey = "woca_test_never_persist_7f31"
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	activator := &recordingActivator{}
	service := NewService(store, activator)

	_, err = service.Connect(t.Context(), ConnectionInput{
		ServerURL: "https://enterprise.example.com/",
		APIKey:    apiKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if activator.request.APIKey != apiKey {
		t.Fatalf("activator API key = %q, want %q", activator.request.APIKey, apiKey)
	}
	if activator.request.BaseURL != "https://enterprise.example.com" {
		t.Fatalf("activator URL = %q", activator.request.BaseURL)
	}
	profile, ok, err := store.ActiveProfile(t.Context())
	if err != nil || !ok {
		t.Fatalf("ActiveProfile() = %#v, %v, %v", profile, ok, err)
	}
	if profile.BaseURL != "https://enterprise.example.com" {
		t.Fatalf("stored URL = %q", profile.BaseURL)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{path, path + "-wal", path + "-shm"} {
		contents, err := os.ReadFile(filename)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(apiKey)) {
			t.Fatalf("%s contains the API key", filepath.Base(filename))
		}
	}
}

func TestConnectReadyWhenActivatorReturnsReady(t *testing.T) {
	service := NewService(openTestStore(t), &recordingActivator{result: auth.ActivationResult{Ready: true}})

	state, err := service.Connect(t.Context(), ConnectionInput{
		ServerURL: "https://Enterprise.Example.com/",
		APIKey:    "issued-bootstrap-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("Connect() = %#v, want ready normalized URL", state)
	}
	state, err = service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("Bootstrap() = %#v, want ready normalized URL after Connect", state)
	}
}

func TestConnectRetainsURLWhenActivationUnavailable(t *testing.T) {
	store := openTestStore(t)
	service := NewService(store, auth.UnavailableActivator{})

	_, err := service.Connect(t.Context(), ConnectionInput{
		ServerURL: "https://enterprise.example.com",
		APIKey:    "issued-bootstrap-key",
	})
	if !errors.Is(err, auth.ErrServerContractUnavailable) {
		t.Fatalf("Connect() error = %v, want unavailable activation error", err)
	}
	profile, ok, err := store.ActiveProfile(t.Context())
	if err != nil || !ok || profile.BaseURL != "https://enterprise.example.com" {
		t.Fatalf("ActiveProfile() = %#v, %v, %v", profile, ok, err)
	}
}

func TestBootstrapRequiresAuthorizationForStoredProfileWithoutSession(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.SaveProfile(t.Context(), "https://enterprise.example.com"); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &recordingActivator{})

	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded || state.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("Bootstrap() = %#v, want authorization required", state)
	}
}

func TestLogoutClearsOnlyInProcessReadiness(t *testing.T) {
	store := openTestStore(t)
	if err := store.PutPreference(t.Context(), "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &recordingActivator{result: auth.ActivationResult{Ready: true}})
	if _, err := service.Connect(t.Context(), ConnectionInput{
		ServerURL: "https://enterprise.example.com",
		APIKey:    "issued-bootstrap-key",
	}); err != nil {
		t.Fatal(err)
	}

	state, err := service.Logout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded || state.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("Logout() = %#v, want authorization required", state)
	}
	if value, ok, err := store.Preference(t.Context(), "theme"); err != nil || !ok || value != "dark" {
		t.Fatalf("Preference() = %q, %v, %v", value, ok, err)
	}
	state, err = service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded || state.ServerURL != "https://enterprise.example.com" {
		t.Fatalf("Bootstrap() = %#v, want authorization required after Logout", state)
	}
}

func TestBootstrapDoesNotCarryReadinessToDifferentActiveProfile(t *testing.T) {
	store := openTestStore(t)
	service := NewService(store, &recordingActivator{result: auth.ActivationResult{Ready: true}})
	if _, err := service.Connect(t.Context(), ConnectionInput{
		ServerURL: "https://enterprise-a.example.com",
		APIKey:    "issued-bootstrap-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveProfile(t.Context(), "https://enterprise-b.example.com"); err != nil {
		t.Fatal(err)
	}

	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateAuthorizationNeeded || state.ServerURL != "https://enterprise-b.example.com" {
		t.Fatalf("Bootstrap() = %#v, want authorization required for the new active profile", state)
	}
}

func TestConnectRejectsBlankAPIKeyBeforeSavingProfile(t *testing.T) {
	store := openTestStore(t)
	service := NewService(store, &recordingActivator{})

	_, err := service.Connect(t.Context(), ConnectionInput{ServerURL: "https://enterprise.example.com"})
	if err == nil {
		t.Fatal("Connect() error = nil")
	}
	if _, ok, err := store.ActiveProfile(t.Context()); err != nil || ok {
		t.Fatalf("ActiveProfile() ok = %v, err = %v", ok, err)
	}
}

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
