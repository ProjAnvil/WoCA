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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
