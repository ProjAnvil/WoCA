package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceFlow(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DeviceAuthResponse{
			DeviceCode:      "dc",
			UserCode:        "ABCD-EFGH",
			VerificationURI: "https://example.com/device",
			Interval:        0,
			ExpiresIn:       60,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&tokenCalls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(Token{AccessToken: "access-123", RefreshToken: "refresh-456"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		ClientID:       "client",
		DeviceEndpoint: srv.URL + "/device",
		TokenEndpoint:  srv.URL + "/token",
		Interval:       1 * time.Millisecond,
	}

	dev, err := BeginDeviceFlow(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dev.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", dev.UserCode)
	}

	tok, err := PollDeviceToken(context.Background(), cfg, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-123" || tok.RefreshToken != "refresh-456" {
		t.Fatalf("token = %+v", tok)
	}
	if atomic.LoadInt32(&tokenCalls) < 3 {
		t.Fatalf("expected at least 3 token polls, got %d", tokenCalls)
	}
}
