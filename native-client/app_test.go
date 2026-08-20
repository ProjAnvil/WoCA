package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
)

type fakeBridgeAPI struct {
	bootstrapState bridge.BootstrapState
	connectState   bridge.BootstrapState
	logoutState    bridge.BootstrapState

	bootstrapCtx    context.Context
	connectCtx      context.Context
	logoutCtx       context.Context
	connectInput    bridge.ConnectionInput
	preferenceKey   string
	preferenceInput bridge.PreferenceInput
	preferenceValue bridge.PreferenceValue
}

func (f *fakeBridgeAPI) Bootstrap(ctx context.Context) (bridge.BootstrapState, error) {
	f.bootstrapCtx = ctx
	return f.bootstrapState, nil
}

func (f *fakeBridgeAPI) Connect(ctx context.Context, input bridge.ConnectionInput) (bridge.BootstrapState, error) {
	f.connectCtx = ctx
	f.connectInput = input
	return f.connectState, nil
}

func (f *fakeBridgeAPI) Logout(ctx context.Context) (bridge.BootstrapState, error) {
	f.logoutCtx = ctx
	return f.logoutState, nil
}

func (f *fakeBridgeAPI) Preference(ctx context.Context, key string) (bridge.PreferenceValue, error) {
	f.preferenceKey = key
	return f.preferenceValue, nil
}

func (f *fakeBridgeAPI) PutPreference(ctx context.Context, input bridge.PreferenceInput) error {
	f.preferenceInput = input
	return nil
}

func TestAppDelegatesWithStartupContext(t *testing.T) {
	service := &fakeBridgeAPI{
		bootstrapState: bridge.BootstrapState{State: bridge.StateUnconfigured},
		connectState:   bridge.BootstrapState{State: bridge.StateReady, ServerURL: "https://enterprise.example.com"},
		logoutState:    bridge.BootstrapState{State: bridge.StateAuthorizationNeeded, ServerURL: "https://enterprise.example.com"},
	}
	app := NewApp(service)
	startupCtx := context.WithValue(context.Background(), "source", "wails")
	app.startup(startupCtx)

	if got, err := app.Bootstrap(); err != nil || got != service.bootstrapState {
		t.Fatalf("Bootstrap() = %#v, %v", got, err)
	}
	input := bridge.ConnectionInput{ServerURL: "https://enterprise.example.com", APIKey: "ephemeral-key"}
	if got, err := app.Connect(input); err != nil || got != service.connectState {
		t.Fatalf("Connect() = %#v, %v", got, err)
	}
	if got, err := app.Logout(); err != nil || got != service.logoutState {
		t.Fatalf("Logout() = %#v, %v", got, err)
	}
	if service.bootstrapCtx != startupCtx || service.connectCtx != startupCtx || service.logoutCtx != startupCtx {
		t.Fatal("App did not delegate using its startup context")
	}
	if service.connectInput != input {
		t.Fatalf("Connect input = %#v, want %#v", service.connectInput, input)
	}
}

func TestAppPreferencesPersistAcrossSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(bridge.NewService(store, nil))
	app.startup(t.Context())
	if err := app.PutPreference(bridge.PreferenceInput{Key: "woca.pref.locale", Value: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := NewApp(bridge.NewService(store, nil))
	got, err := restarted.Preference("woca.pref.locale")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Value != "en" {
		t.Fatalf("Preference() = %#v, want persisted locale", got)
	}

	missing, err := restarted.Preference("woca.pref.model")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Found || missing.Value != "" {
		t.Fatalf("missing Preference() = %#v, want distinguishable missing value", missing)
	}
}

func TestAppShutdownRunsCleanupOnce(t *testing.T) {
	cleanupCalls := 0
	app := newAppWithShutdown(nil, func() { cleanupCalls++ })

	app.shutdown(t.Context())
	app.shutdown(t.Context())

	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}
