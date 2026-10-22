package websearch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenericParsesResults(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"One","url":"https://one.example","snippet":"first"}]}`))
	}))
	defer srv.Close()

	p := NewGeneric(GenericOptions{Endpoint: srv.URL})
	got, err := p.Search(t.Context(), "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "One" || got[0].URL != "https://one.example" {
		t.Fatalf("results = %+v", got)
	}
	if p.ID() != genericID {
		t.Errorf("ID = %q, want %q", p.ID(), genericID)
	}
}

// TestGenericRateLimitIsClassified: a configured endpoint can throttle too, and
// the router can only cool it down if the adapter says so.
func TestGenericRateLimitIsClassified(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewGeneric(GenericOptions{Endpoint: srv.URL})
	_, err := p.Search(t.Context(), "go", 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 = %v, want ErrRateLimited", err)
	}
	if !Failover(err) {
		t.Error("429 must be failover-able")
	}
}

func TestGenericServerErrorIsNotRateLimit(t *testing.T) {
	withPermissiveEndpoints(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewGeneric(GenericOptions{Endpoint: srv.URL})
	_, err := p.Search(t.Context(), "go", 0)
	if err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("500 = %v, want a plain error", err)
	}
}

func TestGenericAvailability(t *testing.T) {
	if NewGeneric(GenericOptions{}).Available() {
		t.Fatal("no endpoint must mean unavailable")
	}
	if NewGeneric(GenericOptions{Endpoint: "http://169.254.169.254/"}).Available() {
		t.Fatal("endpoint must pass the SSRF policy")
	}
	withPermissiveEndpoints(t)
	if !NewGeneric(GenericOptions{Endpoint: "https://search.example.com"}).Available() {
		t.Fatal("valid endpoint should be available")
	}
}
