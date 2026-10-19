package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
)

// TestNewContractHTTP pins the contract-http dispatch: New must build a
// working Sandbox executor against a live contract v2 backend.
func TestNewContractHTTP(t *testing.T) {
	addr := fakesandbox.Addr(t)
	x, err := New(Config{
		Type:     "contract-http",
		Endpoint: addr,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New(contract-http): %v", err)
	}
	res, err := x.Exec(t.Context(), executor.ExecRequest{Name: "echo", Args: []string{"registry"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "registry" {
		t.Fatalf("Exec stdout = %q, want %q", res.Stdout, "registry")
	}
	if c, ok := x.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestNewCfbridge pins the cfbridge dispatch: New must provision a
// sandbox via the bridge create endpoint and yield a closeable Executor.
func TestNewCfbridge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandbox", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("Authorization = %q, want Bearer k1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sbx_r"}`))
	})
	mux.HandleFunc("DELETE /v1/sandbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	x, err := New(Config{
		Type:     "cfbridge",
		Endpoint: srv.URL,
		APIKey:   "k1",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New(cfbridge): %v", err)
	}
	if c, ok := x.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	} else {
		t.Fatal("cfbridge executor must implement io.Closer")
	}
}

// TestNewUnknownType: types without a registered provider error out and
// name the offending type.
func TestNewUnknownType(t *testing.T) {
	for _, ty := range []string{"e2b", "daytona", "modal", "microsandbox", "sidecar", "bogus", ""} {
		_, err := New(Config{Type: ty, Endpoint: "http://sandbox.local"})
		if err == nil {
			t.Fatalf("New(type=%q): want error, got nil", ty)
		}
		if ty != "" && !strings.Contains(err.Error(), ty) {
			t.Errorf("New(type=%q) error %q does not mention the type", ty, err)
		}
	}
}

// TestNewInvalidEndpoint: Endpoint must parse as an absolute URL before
// any provider is constructed.
func TestNewInvalidEndpoint(t *testing.T) {
	for _, ep := range []string{"", "not a url", "localhost:8080", "http://", "://x"} {
		_, err := New(Config{Type: "contract-http", Endpoint: ep})
		if err == nil {
			t.Fatalf("New(endpoint=%q): want error, got nil", ep)
		}
	}
}
