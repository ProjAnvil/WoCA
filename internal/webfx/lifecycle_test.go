package webfx

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func listenEphemeral(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeWaitsForInFlightRequestDuringGrace(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	r := New(Options{Grace: 5 * time.Second}) // long enough that the test controls the timing
	r.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "part")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(inFlight)
		<-release
		_, _ = io.WriteString(w, "-done")
	}))

	ln := listenEphemeral(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Serve(ctx, ln) }()

	done := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			done <- "err:" + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		done <- string(body)
	}()

	<-inFlight
	cancel() // shutdown begins while /slow is still streaming
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if body := <-done; body != "part-done" {
		t.Fatalf("in-flight request severed by shutdown: body = %q", body)
	}
}

func TestServeGraceCutsOffHungHandler(t *testing.T) {
	block := make(chan struct{})
	r := New(Options{Grace: 100 * time.Millisecond})
	r.GET("/hang", func(c *Ctx) { <-block; c.Status(200) })

	ln := listenEphemeral(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Serve(ctx, ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/hang")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-errCh: // grace expired while the handler hung — Shutdown returned
	case <-time.After(2 * time.Second):
		t.Fatal("grace not enforced")
	}
	close(block)
}

func TestRunBindsAddrAndServes(t *testing.T) {
	ln := listenEphemeral(t)
	addr := ln.Addr().String()
	_ = ln.Close() // Run binds the address itself

	r := New(Options{Addr: addr})
	r.GET("/ping", func(c *Ctx) { c.Status(200) })
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/ping")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("Run never answered /ping")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSPAFallback(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>shell</html>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	h := SPA(fsys)

	for _, tc := range []struct {
		method, path string
		wantStatus   int
		wantBody     string
	}{
		{"GET", "/", 200, "<html>shell</html>"},
		{"GET", "/app.js", 200, "console.log(1)"},
		{"GET", "/settings/memory", 200, "<html>shell</html>"}, // client-side route
		{"GET", "/api/whatever", 404, ""},                      // SPA never answers API paths
		{"POST", "/", 404, ""},                                 // non-GET/HEAD (original semantics: NotFound)
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
		}
		if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s %s: body = %q", tc.method, tc.path, rec.Body.String())
		}
	}
}

func TestSwapPlaceholderThenReal(t *testing.T) {
	swap := NewSwap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("starting"))
	}))
	rec := httptest.NewRecorder()
	swap.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Body.String() != "starting" {
		t.Fatalf("placeholder not served: %q", rec.Body.String())
	}

	swap.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("real"))
	}))
	rec = httptest.NewRecorder()
	swap.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Body.String() != "real" {
		t.Fatalf("swapped handler not served: %q", rec.Body.String())
	}
}

// TestSwapConcurrentLoadsUnderRace: concurrent requests during swaps must
// each see exactly one complete handler (run under -race in CI).
func TestSwapConcurrentLoadsUnderRace(t *testing.T) {
	mk := func(tag string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(tag))
		})
	}
	swap := NewSwap(mk("a"))
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				rec := httptest.NewRecorder()
				swap.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
				body := rec.Body.String()
				if body != "a" && body != "b" {
					t.Errorf("torn handler response: %q", body)
					return
				}
			}
		})
	}
	for range 200 {
		swap.Store(mk("a"))
		swap.Store(mk("b"))
	}
	close(done)
	wg.Wait()
}

func TestHealthzPayloadContract(t *testing.T) {
	// The payload contract every woca role serves today: Content-Type
	// "application/json" (no charset) and this exact JSON object.
	rec := httptest.NewRecorder()
	Healthz("woca-edge").ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body: %v", err)
	}
	if v["ok"] != true || v["service"] != "woca-edge" {
		t.Fatalf("payload = %v", v)
	}
}
