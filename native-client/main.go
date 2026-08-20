package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
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

type productionCredentials struct{}

const singleInstanceUniqueID = "com.projanvil.woca.enterprise.native-client"

func (productionCredentials) Credentials(context.Context, string, *url.URL) (transport.Credentials, error) {
	return transport.Credentials{}, authorizationRequiredError{Code: "authorization_required"}
}

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
			Assets:  assets,
			Handler: handler,
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

	service := bridge.NewService(store, auth.UnavailableActivator{})
	credentials, err := configureCredentials(context.Background(), service, developmentURL)
	if err != nil {
		return fmt.Errorf("configure development server: %w", err)
	}

	app := newAppWithShutdown(service, closeStore)
	handler := transport.New(store, credentials, nil)
	return runApp(buildOptions(app, frontendAssets(), handler))
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
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
