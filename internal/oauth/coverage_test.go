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
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
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
	if atomic.LoadInt32(&calls) < 2 || len(statuses) == 0 {
		t.Fatalf("calls=%d statuses=%v", calls, statuses)
	}

	// A transport failure during polling surfaces directly.
	cfg3 := Config{ClientID: "c", DeviceEndpoint: srv.URL, TokenEndpoint: "http://127.0.0.1:1", Interval: time.Millisecond}
	if _, err := PollDeviceToken(ctx, cfg3, dev2, nil); err == nil {
		t.Fatal("expected poll transport error")
	}

	// An unknown OAuth error aborts the poll.
	var n int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
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
