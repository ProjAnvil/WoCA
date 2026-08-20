package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestBuildOptionsUsesSharedAssetsTransportAndBridge(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("ok")}}
	handlerCalled := false
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalled = true })
	app := &App{}

	opts := buildOptions(app, assets, handler)

	if opts.Title != "Woca Enterprise" {
		t.Fatalf("Title = %q", opts.Title)
	}
	if opts.AssetServer == nil || opts.AssetServer.Assets == nil || opts.AssetServer.Handler == nil {
		t.Fatal("asset server not wired")
	}
	opts.AssetServer.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/test", nil))
	if !handlerCalled {
		t.Fatal("asset server did not use the supplied transport handler")
	}
	if len(opts.Bind) != 1 || opts.Bind[0] != app {
		t.Fatalf("Bind = %#v", opts.Bind)
	}
}

func TestBuildOptionsConfiguresWindowAndLifecycle(t *testing.T) {
	closed := false
	app := newAppWithShutdown(nil, func() { closed = true })
	opts := buildOptions(app, fstest.MapFS{}, http.NotFoundHandler())

	if opts.Width != 1280 || opts.Height != 800 {
		t.Fatalf("initial size = %dx%d", opts.Width, opts.Height)
	}
	if opts.MinWidth != 1024 || opts.MinHeight != 640 {
		t.Fatalf("minimum size = %dx%d", opts.MinWidth, opts.MinHeight)
	}
	if opts.BackgroundColour == nil || *opts.BackgroundColour != (options.RGBA{R: 24, G: 24, B: 27, A: 255}) {
		t.Fatalf("BackgroundColour = %#v", opts.BackgroundColour)
	}
	if opts.OnStartup == nil || opts.OnShutdown == nil {
		t.Fatal("lifecycle callbacks not wired")
	}
	startupCtx := context.WithValue(context.Background(), "source", "test")
	opts.OnStartup(startupCtx)
	if app.ctx != startupCtx {
		t.Fatal("OnStartup did not store the Wails context")
	}
	opts.OnShutdown(context.Background())
	if !closed {
		t.Fatal("OnShutdown did not close application resources")
	}
}

func TestProductionCredentialsFailClosed(t *testing.T) {
	credentials, err := (productionCredentials{}).Credentials(
		t.Context(),
		http.MethodGet,
		&url.URL{Scheme: "https", Host: "enterprise.example.com", Path: "/api/models"},
	)
	var authorizationError authorizationRequiredError
	if !errors.As(err, &authorizationError) {
		t.Fatalf("Credentials() error = %v, want structured authorization error", err)
	}
	if credentials.Scheme != "" || credentials.Session != "" || credentials.Proof != "" {
		t.Fatalf("Credentials() = %#v, want no production credentials", credentials)
	}
}

func TestRunWithClosesStoreWhenWailsReturns(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	runnerError := errors.New("Wails startup failed")

	err = runWith(
		func() (*configstore.Store, error) { return store, nil },
		func(*options.App) error { return runnerError },
		"",
	)
	if !errors.Is(err, runnerError) {
		t.Fatalf("runWith() error = %v", err)
	}
	if _, _, err := store.ActiveProfile(t.Context()); err == nil {
		t.Fatal("store remains open after Wails returned")
	}
}

func TestRunWithRejectsUnsafeDevelopmentOverrideAndClosesStore(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	runnerCalled := false

	err = runWith(
		func() (*configstore.Store, error) { return store, nil },
		func(*options.App) error { runnerCalled = true; return nil },
		"https://localhost:9090",
	)
	if err == nil {
		t.Fatal("runWith() error = nil")
	}
	if runnerCalled {
		t.Fatal("Wails runner called for unsafe development override")
	}
	if _, _, err := store.ActiveProfile(t.Context()); err == nil {
		t.Fatal("store remains open after development override failure")
	}
}
