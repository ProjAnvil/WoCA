package main

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
)

type fakeBridgeAPI struct {
	bootstrapState bridge.BootstrapState
	connectState   bridge.BootstrapState
	logoutState    bridge.BootstrapState

	bootstrapCtx     context.Context
	connectCtx       context.Context
	logoutCtx        context.Context
	connectInput     bridge.ConnectionInput
	preferenceKey    string
	preferenceInput  bridge.PreferenceInput
	preferenceValue  bridge.PreferenceValue
	preferenceResult bridge.PreferenceWriteResult
	setPreferenceFn  func(context.Context, bridge.PreferenceInput) (bridge.PreferenceWriteResult, error)
	preferenceMu     sync.Mutex
	preferenceCalls  int
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

func (f *fakeBridgeAPI) SetPreference(ctx context.Context, input bridge.PreferenceInput) (bridge.PreferenceWriteResult, error) {
	f.preferenceMu.Lock()
	f.preferenceInput = input
	f.preferenceCalls++
	f.preferenceMu.Unlock()
	if f.setPreferenceFn != nil {
		return f.setPreferenceFn(ctx, input)
	}
	return f.preferenceResult, nil
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
	result, err := app.SetPreference(bridge.PreferenceInput{Key: "woca.pref.locale", Value: "en", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Revision != 1 {
		t.Fatalf("SetPreference() = %#v, want applied revision 1", result)
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

func TestAppShutdownWaitsForPreferenceWriteAndRejectsNewCalls(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	cleanup := make(chan struct{})
	service := &fakeBridgeAPI{
		setPreferenceFn: func(_ context.Context, input bridge.PreferenceInput) (bridge.PreferenceWriteResult, error) {
			close(entered)
			<-release
			return bridge.PreferenceWriteResult{Revision: input.Revision, Applied: true}, nil
		},
	}
	app := newAppWithShutdown(service, func() { close(cleanup) })

	writeDone := make(chan error, 1)
	go func() {
		_, err := app.SetPreference(bridge.PreferenceInput{Key: "woca.pref.model", Value: "slow-model", Revision: 1})
		writeDone <- err
	}()
	<-entered

	shutdownDone := make(chan struct{})
	go func() {
		app.shutdown(t.Context())
		close(shutdownDone)
	}()
	for {
		app.preferenceMu.Lock()
		closing := app.preferenceClosing
		app.preferenceMu.Unlock()
		if closing {
			break
		}
		runtime.Gosched()
	}
	_, err := app.SetPreference(bridge.PreferenceInput{
		Key: "woca.pref.model", Value: "must-not-enter-bridge", Revision: 2,
	})
	if err == nil || err.Error() != "native application is shutting down" {
		t.Fatalf("SetPreference() while closing error = %v", err)
	}
	select {
	case <-cleanup:
		t.Fatal("shutdown closed SQLite while a preference write was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	<-shutdownDone
	select {
	case <-cleanup:
	default:
		t.Fatal("shutdown did not run cleanup after the preference write finished")
	}

	service.preferenceMu.Lock()
	defer service.preferenceMu.Unlock()
	if service.preferenceCalls != 1 {
		t.Fatalf("bridge preference calls = %d, want 1", service.preferenceCalls)
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
