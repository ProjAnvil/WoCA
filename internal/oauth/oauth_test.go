package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceFlow(t *testing.T) {
	var tokenCalls atomic.Int32
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
		n := tokenCalls.Add(1)
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
	if tokenCalls.Load() < 3 {
		t.Fatalf("expected at least 3 token polls, got %d", tokenCalls.Load())
	}
}

func TestBeginDeviceFlowErrorsAndScopes(t *testing.T) {
	ctx := context.Background()
	// Non-200 status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	cfg := Config{ClientID: "c", DeviceEndpoint: srv.URL, TokenEndpoint: srv.URL}
	if _, err := BeginDeviceFlow(ctx, cfg); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("status error = %v", err)
	}
	srv.Close()

	// Undecodable body.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt_Fprint(w, "not json")
	}))
	defer srv2.Close()
	cfg2 := Config{ClientID: "c", DeviceEndpoint: srv2.URL, TokenEndpoint: srv2.URL}
	if _, err := BeginDeviceFlow(ctx, cfg2); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("decode error = %v", err)
	}
	srv2.Close()

	// Transport failure.
	cfg3 := Config{ClientID: "c", DeviceEndpoint: "http://127.0.0.1:1/d", TokenEndpoint: "http://127.0.0.1:1/t"}
	if _, err := BeginDeviceFlow(ctx, cfg3); err == nil {
		t.Fatal("expected transport error")
	}

	// Success with scopes: the form carries them.
	var gotScope, gotMethod string
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = r.ParseForm()
		gotScope = r.PostForm.Get("scope")
		json.NewEncoder(w).Encode(DeviceAuthResponse{DeviceCode: "dc", UserCode: "UC", Interval: 0, ExpiresIn: 60})
	}))
	defer srv4.Close()
	cfg4 := Config{ClientID: "c", Scopes: []string{"a", "b"}, DeviceEndpoint: srv4.URL, TokenEndpoint: srv4.URL}
	dev, err := BeginDeviceFlow(ctx, cfg4)
	if err != nil || dev.DeviceCode != "dc" {
		t.Fatalf("success = %+v %v", dev, err)
	}
	if gotMethod != http.MethodPost || gotScope != "a b" {
		t.Fatalf("method/scope = %q/%q", gotMethod, gotScope)
	}
}

func fmt_Fprint(w http.ResponseWriter, s string) { _, _ = w.Write([]byte(s)) }

func TestPollDeviceTokenExpiryAndCancel(t *testing.T) {
	ctx := context.Background()
	// Expired response: the loop exits immediately.
	dev := &DeviceAuthResponse{DeviceCode: "dc", ExpiresIn: 0}
	cfg := Config{ClientID: "c", DeviceEndpoint: "http://127.0.0.1:1", TokenEndpoint: "http://127.0.0.1:1"}
	if _, err := PollDeviceToken(ctx, cfg, dev, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry = %v", err)
	}

	// Pending forever with a cancelled context.
	dev2 := &DeviceAuthResponse{DeviceCode: "dc", ExpiresIn: 3600}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer srv.Close()
	cfg2 := Config{ClientID: "c", DeviceEndpoint: srv.URL, TokenEndpoint: srv.URL, Interval: 10 * time.Millisecond}
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	var statuses []string
	if _, err := PollDeviceToken(cctx, cfg2, dev2, func(s string) { statuses = append(statuses, s) }); err == nil {
		t.Fatal("expected ctx error")
	}
	if calls.Load() < 2 || len(statuses) == 0 {
		t.Fatalf("calls=%d statuses=%v", calls.Load(), statuses)
	}

	// A transport failure during polling surfaces directly.
	cfg3 := Config{ClientID: "c", DeviceEndpoint: srv.URL, TokenEndpoint: "http://127.0.0.1:1", Interval: time.Millisecond}
	if _, err := PollDeviceToken(ctx, cfg3, dev2, nil); err == nil {
		t.Fatal("expected poll transport error")
	}

	// An unknown OAuth error aborts the poll.
	var n atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
	}))
	defer srv2.Close()
	cfg4 := Config{ClientID: "c", DeviceEndpoint: srv2.URL, TokenEndpoint: srv2.URL, Interval: time.Millisecond}
	if _, err := PollDeviceToken(ctx, cfg4, dev2, nil); err == nil {
		t.Fatal("expected invalid_client error")
	}
}
