package relayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
func newGetRequest(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }
func contains(s, sub string) bool             { return strings.Contains(s, sub) }

func TestServiceRunStartsAndStopsTunnel(t *testing.T) {
	relay := newFakeRelay(t)
	st := openTestSettings(t)
	if err := SaveConfig(st, relay.srv.URL, "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	svc.SetHandler(http.NotFoundHandler())
	ctx := t.Context()
	go svc.Run(ctx, st, testLogger())

	waitFor(t, "first poll", func() bool { return relay.polls.Load() > 0 })
	if got := svc.Status(); !got.Connected || !got.Configured || got.KeyPrefix != "wcr_test" {
		t.Fatalf("status: %+v", got)
	}
	svc.Disconnect()
	waitFor(t, "disconnect", func() bool { return !svc.Status().Connected })
}

func TestServiceStatusWithoutConfig(t *testing.T) {
	svc := NewService()
	if got := svc.Status(); got.Configured || got.Connected {
		t.Fatalf("zero-config status: %+v", got)
	}
}

func TestServiceRunWithoutConfigIsNoop(t *testing.T) {
	st := openTestSettings(t)
	svc := NewService()
	svc.SetSettings(st)
	svc.SetHandler(http.NotFoundHandler())
	done := make(chan struct{})
	go func() {
		svc.Run(context.Background(), st, testLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unconfigured Run did not return (must be a no-op)")
	}
}

func TestServiceStatusHandler(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://relay.example.com", "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	rec := newRecorder()
	svc.StatusHandler().ServeHTTP(rec, newGetRequest("/api/relay/status"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status handler: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"configured":true`, `"key_prefix":"wcr_test"`} {
		if !contains(body, want) {
			t.Fatalf("status body %s missing %s", body, want)
		}
	}
}

// TestLocalSurfaceRejectsRemote guards the loopback-trust posture (design
// §3.2/§6.4): the tunnel forwards every /api/* request to the full handler,
// so without this check a session-authenticated remote client could POST
// /api/relay/disconnect and kill the bridge's own tunnel. The marker header
// is forgery-proof — execute() strips every X-Woca-* from parked requests
// before injecting it itself.
func TestLocalSurfaceRejectsRemote(t *testing.T) {
	svc := NewService()

	remoteGet := newGetRequest("/api/relay/status")
	remoteGet.Header.Set(remoteHeader, "1")
	rec := newRecorder()
	svc.StatusHandler().ServeHTTP(rec, remoteGet)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote status: %d, want 403", rec.Code)
	}

	remotePost := httptest.NewRequest(http.MethodPost, "/api/relay/disconnect", nil)
	remotePost.Header.Set(remoteHeader, "1")
	rec = newRecorder()
	svc.DisconnectHandler().ServeHTTP(rec, remotePost)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote disconnect: %d, want 403", rec.Code)
	}

	// Without the marker the local posture is unchanged.
	rec = newRecorder()
	svc.DisconnectHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/relay/disconnect", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("local disconnect: %d, want 204", rec.Code)
	}
}

func TestServiceRunWaitsForHandler(t *testing.T) {
	relay := newFakeRelay(t)
	st := openTestSettings(t)
	if err := SaveConfig(st, relay.srv.URL, "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	// Run starts before SetHandler (wiring drift defense): it waits for
	// the handler instead of racing ahead with a nil one.
	ctx := t.Context()
	done := make(chan struct{})
	go func() {
		svc.Run(ctx, st, testLogger())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Run returned before a handler was wired")
	case <-time.After(150 * time.Millisecond):
	}
	svc.SetHandler(http.NotFoundHandler())
	waitFor(t, "first poll after handler wiring", func() bool { return relay.polls.Load() > 0 })
	if got := svc.Status(); !got.Connected {
		t.Fatalf("status after handler wiring: %+v", got)
	}
}

func TestServiceRunHandlerWaitCancel(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "http://127.0.0.1:1", "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	// No handler ever arrives; the context cancel ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx, st, testLogger())
		close(done)
	}()
	time.Sleep(120 * time.Millisecond) // inside the wait loop
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run stuck in the handler wait after cancel")
	}
}

func TestServiceStatusTruncatesKeyPrefix(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://relay.example.com", "wcr_abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	if got := svc.Status(); got.KeyPrefix != "wcr_abcd" {
		t.Fatalf("key prefix %q, want the first 8 chars", got.KeyPrefix)
	}
}

func TestServiceStatusAfterTerminalRun(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusGone) // revoked: terminal
	st := openTestSettings(t)
	if err := SaveConfig(st, relay.srv.URL, "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	svc.SetHandler(http.NotFoundHandler())
	done := make(chan struct{})
	go func() {
		svc.Run(context.Background(), st, testLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal run did not return")
	}
	// The tunnel is gone but its last snapshot still reports the terminal
	// state (the status page reads 'key revoked' after the process stops
	// polling).
	got := svc.Status()
	if got.Connected || !got.Revoked || got.LastError != "key revoked" || !got.Configured {
		t.Fatalf("post-terminal status: %+v", got)
	}
}
