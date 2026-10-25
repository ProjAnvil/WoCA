package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/transport"
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

func (f *fakeBridgeAPI) ConnectRelay(ctx context.Context, input bridge.ConnectionInput) (bridge.BootstrapState, error) {
	f.connectCtx = ctx
	f.connectInput = input
	return f.connectState, nil
}

func (f *fakeBridgeAPI) ConnectEmbedded(ctx context.Context) (bridge.BootstrapState, error) {
	f.connectCtx = ctx
	return f.connectState, nil
}

func (f *fakeBridgeAPI) Logout(ctx context.Context) (bridge.BootstrapState, error) {
	f.logoutCtx = ctx
	return f.logoutState, nil
}

func (f *fakeBridgeAPI) Profiles(context.Context) ([]bridge.ProfileSummary, error) {
	return nil, nil
}

func (f *fakeBridgeAPI) ResumeProfile(context.Context, int64) (bridge.BootstrapState, error) {
	return bridge.BootstrapState{}, nil
}

func (f *fakeBridgeAPI) DeleteProfile(context.Context, int64) (bridge.ProfileDeletion, error) {
	return bridge.ProfileDeletion{}, nil
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
	startupCtx := context.WithValue(t.Context(), "source", "wails")
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

type blockingCancellationRegistry struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCancellationRegistry) Cancel(string) bool { return false }
func (b *blockingCancellationRegistry) CancelAll() {
	close(b.entered)
	<-b.release
}

func TestAppShutdownRejectsPreferenceCallsWhileCancellingRequests(t *testing.T) {
	cancellations := &blockingCancellationRegistry{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := &fakeBridgeAPI{}
	app := newAppWithCancellation(service, cancellations, nil)
	done := make(chan struct{})
	go func() {
		app.shutdown(t.Context())
		close(done)
	}()
	<-cancellations.entered

	_, err := app.SetPreference(bridge.PreferenceInput{
		Key: "woca.pref.model", Value: "must-not-enter-bridge", Revision: 1,
	})
	close(cancellations.release)
	<-done

	if err == nil || err.Error() != "native application is shutting down" {
		t.Fatalf("SetPreference() while cancellation drains error = %v", err)
	}
	service.preferenceMu.Lock()
	defer service.preferenceMu.Unlock()
	if service.preferenceCalls != 0 {
		t.Fatalf("bridge preference calls = %d, want 0", service.preferenceCalls)
	}
}

type appTestCredentials struct{}

func (appTestCredentials) Credentials(context.Context, string, *url.URL) (transport.Credentials, error) {
	return transport.Credentials{}, nil
}

func TestAppCancelRequestReleasesWailsLikeBackgroundRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SaveProfile(t.Context(), upstream.URL); err != nil {
		t.Fatal(err)
	}
	registry := transport.NewCancellationRegistry()
	app := newAppWithCancellation(nil, registry, nil)
	handler := transport.NewWithCancellation(store, appTestCredentials{}, nil, registry)
	id := strings.Repeat("N", 43)
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/events", nil)
	request.Header.Set(transport.RequestIDHeader, id)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}

	if !app.CancelRequest(id) {
		t.Fatal("CancelRequest() did not accept the active request ID")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("App.CancelRequest did not release the upstream request")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after App.CancelRequest")
	}
}

func TestAppShutdownCancelsRequestsBeforeCleanupAndRejectsFutureCancellation(t *testing.T) {
	registry := transport.NewCancellationRegistry()
	id := strings.Repeat("O", 43)
	if !registry.Cancel(id) {
		t.Fatal("precondition pending cancellation failed")
	}
	cleanup := make(chan struct{})
	cleanupRanBeforeCancellation := false
	app := newAppWithCancellation(nil, registry, func() {
		cleanupRanBeforeCancellation = registry.Cancel(strings.Repeat("P", 43))
		close(cleanup)
	})

	app.shutdown(t.Context())

	select {
	case <-cleanup:
	default:
		t.Fatal("shutdown cleanup did not run")
	}
	if cleanupRanBeforeCancellation {
		t.Fatal("cleanup ran before CancelAll closed the registry")
	}
	if app.CancelRequest(strings.Repeat("P", 43)) {
		t.Fatal("CancelRequest() admitted work after shutdown")
	}
}

func TestAppCancelRequestIsHarmlessWithoutRegistry(t *testing.T) {
	if NewApp(nil).CancelRequest(strings.Repeat("Q", 43)) {
		t.Fatal("CancelRequest() succeeded without a native cancellation registry")
	}
}

// EdgeBridges (v1.6 entry ②): the pre-auth inventory call goes to the edge's
// own route and surfaces its list; garbage URLs are rejected locally.
func TestEdgeBridges(t *testing.T) {
	served := make(chan []EdgeBridge, 1)
	served <- []EdgeBridge{{BridgeID: "br_one", Live: true}, {BridgeID: "br_two"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(<-served)
	}))
	defer ts.Close()

	app := NewApp(&fakeBridgeAPI{})
	got, err := app.EdgeBridges(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].BridgeID != "br_one" || !got[0].Live {
		t.Fatalf("bridges = %+v", got)
	}
	if _, err := app.EdgeBridges("not a url"); err == nil {
		t.Fatal("garbage URL accepted")
	}
}

// fakeStreamSource exercises StreamCredentials: remote targets get the
// session material, loopback targets get nothing.
type fakeStreamSource struct{}

func (fakeStreamSource) Credentials(_ context.Context, _ string, target *url.URL) (transport.Credentials, error) {
	return transport.Credentials{Scheme: "Bearer", Session: "sess-" + target.Host}, nil
}

func TestStreamCredentials(t *testing.T) {
	app := NewApp(&fakeBridgeAPI{})

	// No source wired: empty material, no error (streams stay relative).
	cred, err := app.StreamCredentials("https://edge.example.com")
	if err != nil || cred.Scheme != "" || cred.Token != "" {
		t.Fatalf("nil source = %+v %v", cred, err)
	}

	app.streamAuth = fakeStreamSource{}
	cred, err = app.StreamCredentials("https://edge.example.com")
	if err != nil || cred.Scheme != "Bearer" || cred.Token != "sess-edge.example.com" {
		t.Fatalf("remote = %+v %v", cred, err)
	}
	cred, err = app.StreamCredentials("http://127.0.0.1:39871")
	if err != nil || cred.Scheme != "" || cred.Token != "" {
		t.Fatalf("loopback must stay anonymous: %+v %v", cred, err)
	}
	if _, err := app.StreamCredentials("garbage"); err == nil {
		t.Fatal("garbage URL accepted")
	}
}

// Connect persists the API key through the key sink on success (§2.2:
// the OS credential vault is the only client-side key storage; session
// expiry re-authenticates from it). The service clears its copy — the
// sink sees the input before that.
func TestConnectStoresKeyOnSuccess(t *testing.T) {
	var stored []string
	app := NewApp(&fakeBridgeAPI{connectState: bridge.BootstrapState{State: bridge.StateReady}})
	app.storeKey = func(baseURL, apiKey string) { stored = append(stored, baseURL+"="+apiKey) }

	if _, err := app.Connect(bridge.ConnectionInput{
		ServerURL: "https://Bridge.Example.com/",
		APIKey:    "woca_key",
	}); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0] != "https://bridge.example.com=woca_key" {
		t.Fatalf("stored = %v", stored)
	}

	// Failures and keyless (embedded) connects never persist.
	app2 := NewApp(&fakeBridgeAPI{connectState: bridge.BootstrapState{State: bridge.StateAuthorizationNeeded}})
	app2.storeKey = func(baseURL, apiKey string) { stored = append(stored, baseURL) }
	_, _ = app2.Connect(bridge.ConnectionInput{ServerURL: "https://b.example.com", APIKey: "k"})
	if len(stored) != 1 {
		t.Fatalf("failed connect persisted: %v", stored)
	}
}

func TestAppConnectRelayDelegatesWithoutStoringKey(t *testing.T) {
	service := &fakeBridgeAPI{connectState: bridge.BootstrapState{State: bridge.StateReady, ServerURL: "https://relay.example.com"}}
	app := NewApp(service)
	var stored []string
	app.storeKey = func(baseURL, apiKey string) { stored = append(stored, baseURL+"="+apiKey) }

	state, err := app.ConnectRelay(bridge.ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != bridge.StateReady || state.ServerURL != "https://relay.example.com" {
		t.Fatalf("state: %+v", state)
	}
	if service.connectInput.APIKey != "wcr_key" {
		t.Fatalf("bridge input: %+v", service.connectInput)
	}
	// Unlike Connect, the wcr_ key is never pushed into the credential
	// vault — the stored relay session token is the only credential.
	if len(stored) != 0 {
		t.Fatalf("relay key reached the vault sink: %v", stored)
	}
}

func TestAppConnectRelayRequiresBridge(t *testing.T) {
	if _, err := NewApp(nil).ConnectRelay(bridge.ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"}); err == nil {
		t.Fatal("nil bridge accepted")
	}
}
