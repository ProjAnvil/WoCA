package relayclient

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/relay"
)

// TestFullChain wires the REAL relay (internal/relay — test-only import)
// and the real tunnel client around a stub bridge handler: the §10
// integration shape — round trip, SSE fidelity, mandatory heartbeat.
func TestFullChain(t *testing.T) {
	ks, err := relay.OpenKeyStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	rl := relay.NewRelay(ks, fstest.MapFS{"index.html": {Data: []byte("app")}}, false, testLogger())
	srv := httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/echo":
			fmt.Fprintf(w, "echo:%s", r.URL.Query().Get("q"))
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: start\n\n")
			// 350ms of silence: the 100ms test heartbeat must fill it.
			time.Sleep(350 * time.Millisecond)
			fmt.Fprint(w, "data: end\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	c := New(Config{
		URL:               srv.URL,
		Key:               raw,
		Handler:           handler,
		Workers:           2,
		HeartbeatInterval: 100 * time.Millisecond,
	}, testLogger())
	ctx := t.Context()
	go c.Run(ctx)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+raw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	// Round trip through login → park → poll → in-process handler →
	// respond → client.
	var body string
	waitFor(t, "echo round trip", func() bool {
		resp, err := client.Get(srv.URL + "/api/echo?q=hi")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		data, _ := io.ReadAll(resp.Body)
		body = string(data)
		return true
	})
	if body != "echo:hi" {
		t.Fatalf("echo: %q", body)
	}

	// SSE: frames pass through and the idle gap carries the mandatory
	// heartbeat ping (design §6.3 — without it idle SSE dies in
	// intermediary 60-100s windows).
	resp, err = client.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	out := string(data)
	for _, want := range []string{"data: start", ": ping", "data: end"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE stream %q missing %q", out, want)
		}
	}
}
