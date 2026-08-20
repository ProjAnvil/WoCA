//go:build dev

package main

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
)

func TestDevelopmentBuildReadsServerEnvironment(t *testing.T) {
	t.Setenv("WOCA_NATIVE_DEV_BFF_URL", "http://localhost:9090")
	if got := developmentServerURL(); got != "http://localhost:9090" {
		t.Fatalf("developmentServerURL() = %q", got)
	}
}

func TestDevelopmentCredentialsRemainEmpty(t *testing.T) {
	provider := developmentCredentials{baseURL: "http://localhost:9090"}
	credentials, err := provider.Credentials(
		t.Context(),
		http.MethodPost,
		&url.URL{Scheme: "http", Host: "localhost:9090", Path: "/api/chat"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Scheme != "" || credentials.Session != "" || credentials.Proof != "" {
		t.Fatalf("Credentials() = %#v, want empty loopback development credentials", credentials)
	}
	if _, err := provider.Credentials(
		t.Context(),
		http.MethodPost,
		&url.URL{Scheme: "https", Host: "enterprise.example.com", Path: "/api/chat"},
	); err == nil {
		t.Fatal("Credentials() allowed a target outside the validated loopback origin")
	}
}

func TestConfigureCredentialsEnablesLoopbackDevelopmentOnlyAfterReadiness(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := bridge.NewService(store, nil)

	provider, err := configureCredentials(t.Context(), service, "http://127.0.0.1:9090")
	if err != nil {
		t.Fatal(err)
	}
	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != bridge.StateReady || state.ServerURL != "http://127.0.0.1:9090" {
		t.Fatalf("Bootstrap() = %#v", state)
	}
	credentials, err := provider.Credentials(t.Context(), http.MethodGet, &url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:9090",
		Path:   "/api/models",
	})
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Scheme != "" || credentials.Session != "" || credentials.Proof != "" {
		t.Fatalf("Credentials() = %#v", credentials)
	}
}
