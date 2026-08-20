package bridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestPreferenceRejectsUnknownAndSensitiveKeys(t *testing.T) {
	service := NewService(openTestStore(t), nil)
	for _, key := range []string{
		"unknown",
		"api_key",
		"woca.accessToken",
		"refresh_token",
		"session",
		"credentials",
		"server_url",
	} {
		t.Run(key, func(t *testing.T) {
			if _, err := service.SetPreference(t.Context(), PreferenceInput{Key: key, Value: "do-not-store", Revision: 1}); err == nil {
				t.Fatalf("SetPreference(%q) error = nil", key)
			}
			if _, err := service.Preference(t.Context(), key); err == nil {
				t.Fatalf("Preference(%q) error = nil", key)
			}
		})
	}
}

func TestPreferenceRejectsInvalidAndOversizedValues(t *testing.T) {
	service := NewService(openTestStore(t), nil)
	for _, input := range []PreferenceInput{
		{Key: "woca-theme", Value: "midnight", Revision: 1},
		{Key: "woca.pref.locale", Value: "fr", Revision: 1},
		{Key: "woca.pref.autoCollapseTools", Value: "1", Revision: 1},
		{Key: "woca.pref.markdownPreview", Value: "yes", Revision: 1},
		{Key: "woca.pref.model", Value: "", Revision: 1},
		{Key: "woca.pref.model", Value: "https://secret.example.com", Revision: 1},
		{Key: "woca.pref.model", Value: strings.Repeat("m", 257), Revision: 1},
		{Key: "woca.pref.permissionMode", Value: "admin", Revision: 1},
	} {
		t.Run(input.Key+"/"+input.Value[:min(len(input.Value), 12)], func(t *testing.T) {
			if _, err := service.SetPreference(t.Context(), input); err == nil {
				t.Fatalf("SetPreference(%q) error = nil", input.Key)
			}
		})
	}
}

func TestPreferenceAcceptsKnownTypedValues(t *testing.T) {
	service := NewService(openTestStore(t), nil)
	inputs := []PreferenceInput{
		{Key: "woca-theme", Value: "dark", Revision: 1},
		{Key: "woca.pref.locale", Value: "zh-CN", Revision: 2},
		{Key: "woca.pref.autoCollapseTools", Value: "false", Revision: 3},
		{Key: "woca.pref.markdownPreview", Value: "true", Revision: 4},
		{Key: "woca.pref.model", Value: "claude-sonnet-4-5", Revision: 5},
		{Key: "woca.pref.permissionMode", Value: "acceptEdits", Revision: 6},
	}
	for _, input := range inputs {
		result, err := service.SetPreference(t.Context(), input)
		if err != nil {
			t.Fatalf("SetPreference(%q) error = %v", input.Key, err)
		}
		if !result.Applied || result.Revision != input.Revision {
			t.Fatalf("SetPreference(%q) = %#v", input.Key, result)
		}
		got, err := service.Preference(t.Context(), input.Key)
		if err != nil || !got.Found || got.Value != input.Value {
			t.Fatalf("Preference(%q) = %#v, %v", input.Key, got, err)
		}
	}
}

func TestSetPreferenceRejectsInvalidRevision(t *testing.T) {
	service := NewService(openTestStore(t), nil)
	for _, revision := range []uint64{0, 9_007_199_254_740_992} {
		if _, err := service.SetPreference(t.Context(), PreferenceInput{
			Key: "woca.pref.model", Value: "model", Revision: revision,
		}); err == nil {
			t.Fatalf("SetPreference(revision %d) error = nil", revision)
		}
	}
}

type blockingPreferenceStore struct {
	*configstore.Store
	blockValue string
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (s *blockingPreferenceStore) PutPreference(ctx context.Context, key, value string) error {
	if value == s.blockValue {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.PutPreference(ctx, key, value)
}

func TestSetPreferencePreventsOlderRevisionFromOverwritingNewer(t *testing.T) {
	base := openTestStore(t)
	store := &blockingPreferenceStore{
		Store: base, blockValue: "newer", entered: make(chan struct{}), release: make(chan struct{}),
	}
	service := NewService(store, nil)
	type outcome struct {
		result PreferenceWriteResult
		err    error
	}
	newerDone := make(chan outcome, 1)
	go func() {
		result, err := service.SetPreference(t.Context(), PreferenceInput{
			Key: "woca.pref.model", Value: "newer", Revision: 2,
		})
		newerDone <- outcome{result, err}
	}()
	<-store.entered
	olderDone := make(chan outcome, 1)
	go func() {
		result, err := service.SetPreference(t.Context(), PreferenceInput{
			Key: "woca.pref.model", Value: "older", Revision: 1,
		})
		olderDone <- outcome{result, err}
	}()
	close(store.release)

	newer := <-newerDone
	older := <-olderDone
	if newer.err != nil || !newer.result.Applied {
		t.Fatalf("newer SetPreference() = %#v, %v", newer.result, newer.err)
	}
	if older.err != nil || older.result.Applied {
		t.Fatalf("older SetPreference() = %#v, %v, want stale no-op", older.result, older.err)
	}
	value, found, err := base.Preference(t.Context(), "woca.pref.model")
	if err != nil || !found || value != "newer" {
		t.Fatalf("Preference() = %q, %v, %v; want newer", value, found, err)
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

func TestApplyDevelopmentOverridePersistsLoopbackAndMarksReady(t *testing.T) {
	store := openTestStore(t)
	service := NewService(store, nil)

	baseURL, err := service.ApplyDevelopmentOverride(t.Context(), "http://LOCALHOST:9090/")
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != "http://localhost:9090" {
		t.Fatalf("ApplyDevelopmentOverride() base URL = %q", baseURL)
	}

	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "http://localhost:9090" {
		t.Fatalf("Bootstrap() = %#v, want ready loopback profile", state)
	}
}

func TestApplyDevelopmentOverrideRejectsUnsafeTargetBeforeSaving(t *testing.T) {
	tests := []string{
		"http://enterprise.example.com",
		"https://localhost:9090",
		"http://localhost:9090/app",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			store := openTestStore(t)
			service := NewService(store, nil)

			if _, err := service.ApplyDevelopmentOverride(t.Context(), rawURL); err == nil {
				t.Fatal("ApplyDevelopmentOverride() error = nil")
			}
			if _, ok, err := store.ActiveProfile(t.Context()); err != nil || ok {
				t.Fatalf("ActiveProfile() ok = %v, err = %v", ok, err)
			}
		})
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
