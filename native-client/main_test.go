package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
	"github.com/projanvil/woca/native-client/internal/transport"
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
	if opts.AssetServer.Middleware == nil {
		t.Fatal("asset server document guard middleware not wired")
	}
	guarded := opts.AssetServer.Middleware(handler)
	guarded.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/", nil))
	if handlerCalled {
		t.Fatal("document guard middleware forwarded a document-shaped API path")
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

func TestBuildOptionsUsesRedactingWailsLogger(t *testing.T) {
	opts := buildOptions(&App{}, fstest.MapFS{}, http.NotFoundHandler())
	assertRedactingOptionsLogger(t, opts)
}

func assertRedactingOptionsLogger(t *testing.T, opts *options.App) {
	t.Helper()
	if opts.Logger == nil {
		t.Fatal("Logger = nil")
	}
	if _, ok := opts.Logger.(*redactingLogger); !ok {
		t.Fatalf("Logger = %T, want redacting logger", opts.Logger)
	}
}

func TestBuildOptionsUsesStableSingleInstanceLock(t *testing.T) {
	first := buildOptions(&App{}, fstest.MapFS{}, http.NotFoundHandler())
	second := buildOptions(&App{}, fstest.MapFS{}, http.NotFoundHandler())

	if first.SingleInstanceLock == nil {
		t.Fatal("SingleInstanceLock = nil")
	}
	if first.SingleInstanceLock.UniqueId == "" {
		t.Fatal("SingleInstanceLock.UniqueId is empty")
	}
	const wantUniqueID = "com.projanvil.woca.enterprise.native-client"
	if first.SingleInstanceLock.UniqueId != wantUniqueID {
		t.Fatalf("SingleInstanceLock.UniqueId = %q, want stable ID %q", first.SingleInstanceLock.UniqueId, wantUniqueID)
	}
	if second.SingleInstanceLock == nil || second.SingleInstanceLock.UniqueId != first.SingleInstanceLock.UniqueId {
		t.Fatalf("SingleInstanceLock is not stable: first=%#v second=%#v", first.SingleInstanceLock, second.SingleInstanceLock)
	}
	if first.SingleInstanceLock.OnSecondInstanceLaunch != nil {
		t.Fatal("unexpected second-instance UI callback")
	}
}

func TestProductionCredentialsFailClosed(t *testing.T) {
	material, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	// Without a stored session the production provider mints nothing: the
	// transport maps the error to a sanitized 401.
	p := transport.NewDPoPCredentials(material, func() (string, bool) { return "", false })
	credentials, err := p.Credentials(
		t.Context(),
		http.MethodGet,
		&url.URL{Scheme: "https", Host: "enterprise.example.com", Path: "/api/models"},
	)
	if !errors.Is(err, transport.ErrNoNativeSession) {
		t.Fatalf("Credentials() error = %v, want ErrNoNativeSession", err)
	}
	if credentials.Scheme != "" || credentials.Session != "" || credentials.Proof != "" {
		t.Fatalf("Credentials() = %#v, want no production credentials", credentials)
	}
}

func TestRunWithClosesStoreWhenWailsReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runnerError := errors.New("Wails startup failed")

	err = runWith(
		func() (string, error) { return path, nil },
		func(gotPath string) (*configstore.Store, error) {
			if gotPath != path {
				t.Fatalf("Open path = %q, want %q", gotPath, path)
			}
			return store, nil
		},
		func(opts *options.App) error {
			assertRedactingOptionsLogger(t, opts)
			return runnerError
		},
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
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runnerCalled := false

	err = runWith(
		func() (string, error) { return path, nil },
		func(string) (*configstore.Store, error) { return store, nil },
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

func TestRunWithOpenFailureStartsFatalWindowWithDatabasePath(t *testing.T) {
	const databasePath = "/Users/test/Library/Application Support/woca-native-client/client.db"
	storeErr := errors.New("injected migration failure containing raw SQL")
	openCalls := 0
	runnerCalls := 0

	err := runWith(
		func() (string, error) { return databasePath, nil },
		func(path string) (*configstore.Store, error) {
			openCalls++
			if path != databasePath {
				t.Fatalf("Open path = %q", path)
			}
			return nil, storeErr
		},
		func(opts *options.App) error {
			runnerCalls++
			assertFatalLocalDataOptions(t, opts, databasePath)
			return nil
		},
		"",
	)
	if err != nil {
		t.Fatalf("runWith() error = %v; fatal window handled the failure", err)
	}
	if openCalls != 1 || runnerCalls != 1 {
		t.Fatalf("calls = Open %d, Wails %d; want exactly one each", openCalls, runnerCalls)
	}
}

func TestRunWithDefaultPathFailureStartsFatalWindowWithUnavailablePath(t *testing.T) {
	pathErr := errors.New("injected home resolution failure")
	openCalled := false

	err := runWith(
		func() (string, error) { return "", pathErr },
		func(string) (*configstore.Store, error) { openCalled = true; return nil, nil },
		func(opts *options.App) error {
			assertFatalLocalDataOptions(t, opts, "")
			return nil
		},
		"",
	)
	if err != nil {
		t.Fatalf("runWith() error = %v; fatal window handled the failure", err)
	}
	if openCalled {
		t.Fatal("Open called after DefaultPath failed")
	}
}

func TestRunWithFatalWindowFailureKeepsStoreAndWailsErrorContext(t *testing.T) {
	storeErr := errors.New("injected SQLite open failure")
	runnerErr := errors.New("injected Wails startup failure")

	err := runWith(
		func() (string, error) { return "/tmp/client.db", nil },
		func(string) (*configstore.Store, error) { return nil, storeErr },
		func(*options.App) error { return runnerErr },
		"",
	)
	if !errors.Is(err, storeErr) || !errors.Is(err, runnerErr) {
		t.Fatalf("runWith() error = %v, want both Store and Wails causes", err)
	}
}

func assertFatalLocalDataOptions(t *testing.T, opts *options.App, wantPath string) {
	t.Helper()
	assertRedactingOptionsLogger(t, opts)
	if len(opts.Bind) != 1 {
		t.Fatalf("Bind = %#v", opts.Bind)
	}
	app, ok := opts.Bind[0].(*App)
	if !ok {
		t.Fatalf("bound bridge = %T, want *App", opts.Bind[0])
	}
	state, err := app.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if state.State != bridge.StateFatalLocalData || state.DatabasePath != wantPath {
		t.Fatalf("Bootstrap() = %#v", state)
	}
	if app.CancelRequest(strings.Repeat("Z", 43)) {
		t.Fatal("fatal local-data app accepted native request cancellation")
	}
	response := httptest.NewRecorder()
	opts.AssetServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("fatal handler status = %d, want 404", response.Code)
	}
}
