package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/devicekey"
	"github.com/projanvil/woca/native-client/internal/transport"
	"github.com/wailsapp/wails/v2"
	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

type authorizationRequiredError struct {
	Code string
}

func (e authorizationRequiredError) Error() string { return e.Code }

const singleInstanceUniqueID = "com.projanvil.woca.enterprise.native-client"

func buildOptions(app *App, assets fs.FS, handler http.Handler) *options.App {
	return &options.App{
		Title:            "Woca Enterprise",
		Width:            1280,
		Height:           800,
		MinWidth:         1024,
		MinHeight:        640,
		BackgroundColour: options.NewRGB(24, 24, 27),
		Logger:           newRedactingLogger(wailslogger.NewDefaultLogger()),
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Handler:    handler,
			Middleware: transport.AssetDocumentGuard,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: singleInstanceUniqueID,
		},
	}
}

func runWith(
	resolvePath func() (string, error),
	openStore func(string) (*configstore.Store, error),
	runApp func(*options.App) error,
	developmentURL string,
) error {
	databasePath, err := resolvePath()
	if err != nil {
		return runFatalLocalDataWindow(runApp, "", err)
	}
	store, err := openStore(databasePath)
	if err != nil {
		return runFatalLocalDataWindow(runApp, databasePath, err)
	}
	var closeOnce sync.Once
	closeStore := func() { closeOnce.Do(func() { _ = store.Close() }) }
	defer closeStore()

	// The device key is the client's cryptographic identity: its DPoP
	// thumbprint is pinned at activation and its wrap key seals the session.
	material, err := devicekey.LoadOrCreate(filepath.Join(filepath.Dir(databasePath), "device.key"))
	if err != nil {
		closeStore()
		return runFatalLocalDataWindow(runApp, databasePath, err)
	}
	// Remote TLS is TOFU-pinned (§2.2): the activation exchange and the
	// bridge login both ride a client that records the first SPKI and
	// hard-rejects any later change.
	pinStore, err := transport.OpenPinStore(filepath.Dir(databasePath))
	if err != nil {
		closeStore()
		return fmt.Errorf("open pin store: %w", err)
	}
	pinnedClient := pinStore.Client()
	vault := bridge.NewDeviceSessionVault(store, material, nil)
	service := bridge.NewService(
		store,
		auth.NewHTTPActivator(pinnedClient, material),
		bridge.WithSessionVault(vault),
	)
	// The session source mirrors the bridge bootstrap decision: the active
	// profile's stored session, if any.
	sessionSource := func() (string, bool) {
		profile, ok, err := store.ActiveProfile(context.Background())
		if err != nil || !ok {
			return "", false
		}
		return vault.Load(context.Background(), profile.ID)
	}
	// The embedded bridge (product design §5.1): install+spawn the bundled
	// binary and adopt it as the zero-login default profile on first run.
	bridgeMgr, bridgeKeys := setupEmbeddedBridge(context.Background(), filepath.Dir(databasePath), func(baseURL string) {
		// Record the embedded origin so the connect page's entry ① can
		// (re-)adopt it at any time (design §6.6).
		service.SetEmbeddedBaseURL(baseURL)
		if _, ok, _ := store.ActiveProfile(context.Background()); !ok {
			_, _ = service.ConnectLoopback(context.Background(), baseURL)
		}
	})
	if bridgeMgr != nil {
		defer func() { _ = bridgeMgr.Stop() }()
	}

	// Credentials: loopback (the embedded bridge) stays anonymous; remote
	// bridges log in with the stored API key (bridge /api/auth/login),
	// enterprise endpoints fall through to the DPoP flow (both direct
	// paths ride the TOFU-pinned client above).
	dpop := transport.NewDPoPCredentials(material, sessionSource)
	credentials, err := configureCredentials(context.Background(), service, developmentURL, func() transport.CredentialProvider {
		if profile, ok, _ := store.ActiveProfile(context.Background()); ok && isLoopbackProfileURL(profile.BaseURL) {
			return loopbackCredentials{}
		}
		if profile, ok, _ := store.ActiveProfile(context.Background()); ok && bridgeKeys != nil {
			base := profile.BaseURL
			return &transport.BridgeCredentials{
				BaseURL: base,
				APIKey:  func() (string, error) { return bridgeKeys.Get(base), nil },
				Client:  pinnedClient,
				DPoP:    dpop,
			}
		}
		return dpop
	})
	if err != nil {
		return fmt.Errorf("configure development server: %w", err)
	}

	cancellations := transport.NewCancellationRegistry()
	app := newAppWithCancellation(service, cancellations, closeStore)
	// Direct SSE connections authenticate with the same Bearer the proxy
	// injects (P1: remote profiles without it got 401 event streams).
	app.streamAuth = credentials
	// Successful connections persist the API key in the credential vault
	// (§2.2): session expiry re-authenticates from the keychain without
	// re-pasting (embedded builds carry no store — nothing to persist).
	if bridgeKeys != nil {
		app.storeKey = func(baseURL, apiKey string) {
			_ = bridgeKeys.Set(baseURL, apiKey)
		}
	}
	handler := transport.NewWithCancellation(store, credentials, nil, cancellations)
	return runApp(buildOptions(app, frontendAssets(), handler))
}

// isLoopbackProfileURL reports whether a profile targets the local
// machine (the embedded bridge — trusted, no login).
func isLoopbackProfileURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func runFatalLocalDataWindow(runApp func(*options.App) error, databasePath string, localDataErr error) error {
	app := NewApp(newFatalLocalDataBridge(databasePath))
	if err := runApp(buildOptions(app, frontendAssets(), http.NotFoundHandler())); err != nil {
		return errors.Join(
			fmt.Errorf("open native configuration: %w", localDataErr),
			fmt.Errorf("start fatal local-data window: %w", err),
		)
	}
	return nil
}

func run() error {
	return runWith(configstore.DefaultPath, configstore.Open, wails.Run, developmentServerURL())
}

func main() {
	// This logger is outside Wails and receives only run/startup failures. Bound
	// bridge inputs remain inside Wails and cannot flow into run's error path.
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
