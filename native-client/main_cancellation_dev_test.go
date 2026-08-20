//go:build dev

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/transport"
	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestRunWithWiresAppCancellationToBackgroundAssetRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	defer upstream.Close()
	databasePath := filepath.Join(t.TempDir(), "client.db")

	err := runWith(
		func() (string, error) { return databasePath, nil },
		configstore.Open,
		func(opts *options.App) error {
			app := opts.Bind[0].(*App)
			id := strings.Repeat("R", 43)
			request := httptest.NewRequest(http.MethodGet, "http://native.local/api/events", nil)
			request.Header.Set(transport.RequestIDHeader, id)
			done := make(chan struct{})
			go func() {
				opts.AssetServer.Handler.ServeHTTP(httptest.NewRecorder(), request)
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				close(release)
				return &timeoutError{"upstream request did not start"}
			}
			if !app.CancelRequest(id) {
				close(release)
				<-done
				return &timeoutError{"App.CancelRequest did not share the handler registry"}
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				close(release)
				return &timeoutError{"upstream context was not cancelled"}
			}
			select {
			case <-done:
				return nil
			case <-time.After(2 * time.Second):
				close(release)
				return &timeoutError{"handler did not return"}
			}
		},
		upstream.URL,
	)
	if err != nil {
		t.Fatal(err)
	}
}

type timeoutError struct{ message string }

func (e *timeoutError) Error() string { return e.message }
