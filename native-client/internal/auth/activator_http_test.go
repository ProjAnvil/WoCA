package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/devicekey"
)

func testMaterial(t *testing.T) devicekey.Material {
	t.Helper()
	m, err := devicekey.LoadOrCreate(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHTTPActivatorExchangesKeyForSession(t *testing.T) {
	material := testMaterial(t)
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/native/activate" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "wsess_aa_bb",
			"expires_at": int64(1700000000000),
		})
	}))
	defer server.Close()

	a := NewHTTPActivator(server.Client(), material)
	res, err := a.Activate(t.Context(), ActivationRequest{
		BaseURL: server.URL,
		APIKey:  "woca_test_key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || res.SessionID != "wsess_aa_bb" || res.ExpiresAt != 1700000000000 {
		t.Fatalf("result = %+v", res)
	}
	if gotBody["api_key"] != "woca_test_key" || gotBody["device_id"] != material.DeviceID {
		t.Fatalf("request body = %+v", gotBody)
	}
	jwk, _ := json.Marshal(gotBody["dpop_jwk"])
	if !strings.Contains(string(jwk), `"P-256"`) {
		t.Fatalf("dpop_jwk = %s", jwk)
	}
}

func TestHTTPActivatorMapsStatuses(t *testing.T) {
	material := testMaterial(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer server.Close()
	a := NewHTTPActivator(server.Client(), material)

	if _, err := a.Activate(t.Context(), ActivationRequest{BaseURL: server.URL, APIKey: "k"}); err != ErrInvalidCredentials {
		t.Fatalf("401 = %v", err)
	}
	if _, err := a.Activate(t.Context(), ActivationRequest{BaseURL: server.URL, APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("502 = %v", err)
	}
	if _, err := a.Activate(t.Context(), ActivationRequest{BaseURL: server.URL, APIKey: "k"}); err != ErrActivationRateLimited {
		t.Fatalf("429 = %v", err)
	}
}

func TestUnavailableActivatorStillFailsClosed(t *testing.T) {
	if _, err := (UnavailableActivator{}).Activate(t.Context(), ActivationRequest{}); err != ErrServerContractUnavailable {
		t.Fatal("unavailable activator no longer fails closed")
	}
	_ = time.Now
}
