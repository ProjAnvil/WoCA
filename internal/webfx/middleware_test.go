package webfx

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/rs/zerolog"
)

func TestRequestIDSanitizeGenerateEcho(t *testing.T) {
	r := New(Options{})
	r.GET("/x", func(c *Ctx) { c.Status(200) })
	r.Use(RequestID())
	h := r.Handler()

	// Inbound valid id is kept and echoed.
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", "job-1234.abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "job-1234.abc" {
		t.Errorf("echo = %q, want job-1234.abc", got)
	}
	if got := RequestIDFrom(req.Context()); got != "" {
		// The middleware stored it on the derived request passed downstream,
		// not on the caller's original — the response header is the contract.
		_ = got
	}

	// Hostile ids are replaced, not echoed verbatim.
	for _, bad := range []string{"", strings.Repeat("x", 65), "a b\nc", "id;path=../../etc"} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("X-Request-Id", bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header().Get("X-Request-Id")
		if got == bad && bad != "" {
			t.Errorf("unsanitized request id echoed: %q", got)
		}
		if got == "" {
			t.Errorf("no request id generated for inbound %q", bad)
		}
		if sanitizeRequestID(got) != got {
			t.Errorf("generated id fails its own sanitizer: %q", got)
		}
	}
}

func TestLoggerFieldsAndRoutePattern(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	r := New(Options{})
	r.Use(RequestID(), Logger(log))
	r.GET("/api/things/{id}", func(c *Ctx) { c.Status(200) })
	r.GET("/api/bad", func(c *Ctx) { c.Error(419, "no") })
	h := r.Handler()

	req := httptest.NewRequest("GET", "/api/things/42", nil)
	req = req.WithContext(tenancy.NewContext(req.Context(), tenancy.Context{TenantID: "t1"}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	for _, want := range []string{`"route":"/api/things/{id}"`, `"status":200`, `"method":"GET"`, `"path":"/api/things/42"`, `"tenant_id":"t1"`, `"request_id":"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %s: %s", want, line)
		}
	}

	// 4xx maps to warn.
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/bad", nil))
	if !strings.Contains(buf.String(), `"level":"warn"`) {
		t.Errorf("4xx should log at warn: %s", buf.String())
	}

	// Unmatched falls back to the fixed label, never a raw path.
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/secret/nope", nil))
	if !strings.Contains(buf.String(), `"route":"unmatched"`) {
		t.Errorf("unmatched route label missing: %s", buf.String())
	}
	if strings.Contains(buf.String(), "/secret/nope") == false {
		// path field carries the raw path on purpose (single-line, not a
		// label); the route label is what metrics/queries key on.
		t.Errorf("raw path should stay in the path field for debugging: %s", buf.String())
	}
}

func TestLoggerCarriesRunScope(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	r := New(Options{})
	r.Use(Logger(log))
	r.GET("/x", func(c *Ctx) { c.Status(200) })
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(telemetry.WithRun(req.Context(), "run-9", "sess-1"))
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), `"run_id":"run-9"`) {
		t.Errorf("run scope missing from log line: %s", buf.String())
	}
}

// sseRecorder is a ResponseWriter that records exactly when frames cross
// the wire, so tests can assert incremental flushing rather than
// buffered-until-close behavior.
type sseRecorder struct {
	httptest.ResponseRecorder
	flushes  int
	writeLog []string
}

func (s *sseRecorder) Write(p []byte) (int, error) {
	s.writeLog = append(s.writeLog, string(p))
	return s.ResponseRecorder.Write(p)
}

func (s *sseRecorder) Flush() {
	s.flushes++
	s.ResponseRecorder.Flush()
}

// TestSSEThroughMiddlewareIsTheAugustRegression: the platform's SSE
// contract is headers-set → first flush → incremental frames, mediated by
// http.NewResponseController, and it must survive every middleware wrapper
// in the chain. A wrapper that breaks Flusher delegation or buffers the
// body fails here.
func newSSERecorder() *sseRecorder {
	s := &sseRecorder{}
	s.Body = &bytes.Buffer{}
	return s
}

func TestSSEThroughMiddlewareIsTheAugustRegression(t *testing.T) {
	r := New(Options{})
	r.Use(RequestID(), Logger(zerolog.Nop()), Recovery(zerolog.Nop()))
	r.Handle("GET /api/runs/{id}/events", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if err := rc.Flush(); err != nil {
			t.Errorf("first flush through middleware: %v", err)
		}
		for i, frame := range []string{"data: 1\n\n", "data: 2\n\n", "data: 3\n\n"} {
			_, _ = w.Write([]byte(frame))
			if err := rc.Flush(); err != nil {
				t.Errorf("frame %d flush: %v", i, err)
			}
		}
	}))

	rec := newSSERecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/runs/r1/events", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q (headers must reach the wire before the first flush)", ct)
	}
	if rec.flushes != 4 {
		t.Fatalf("flushes = %d, want 4 (one per frame + the header flush); a wrapper is swallowing Flush", rec.flushes)
	}
	if rec.Body.String() != "data: 1\n\ndata: 2\n\ndata: 3\n\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// hijackableRecorder proves Hijack delegation reaches the underlying
// writer (bridge tunnels depend on it).
type hijackableRecorder struct {
	httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, c2 := net.Pipe()
	return c1, bufio.NewReadWriter(bufio.NewReader(c2), bufio.NewWriter(c2)), nil
}

func TestCaptureWriterHijackPassthrough(t *testing.T) {
	rec := &hijackableRecorder{}
	sw := &captureWriter{ResponseWriter: rec, status: 200}
	_, _, err := sw.Hijack()
	if err != nil {
		t.Fatalf("hijack through captureWriter: %v", err)
	}
	if !rec.hijacked {
		t.Fatal("hijack did not reach the underlying writer")
	}

	// Non-hijackable underlying writer gets a clean error, not a panic.
	sw2 := &captureWriter{ResponseWriter: httptest.NewRecorder(), status: 200}
	if _, _, err := sw2.Hijack(); err == nil {
		t.Fatal("hijack on non-hijackable writer should error")
	}
}

func TestCaptureWriterReaderFromAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &captureWriter{ResponseWriter: rec, status: 200}
	// Unwrap lets ResponseController and stdlib reach through.
	if sw.Unwrap() != rec {
		t.Fatal("Unwrap must return the underlying writer")
	}
	n, err := sw.Write([]byte("hello "))
	if err != nil || n != 6 {
		t.Fatalf("write: %d %v", n, err)
	}
	if sw.bytes != 6 || !sw.wroteHeader() || sw.status != 200 {
		t.Fatalf("capture state: bytes=%d wrote=%v status=%d", sw.bytes, sw.wroteHeader(), sw.status)
	}
	sw.WriteHeader(500) // late WriteHeader must not overwrite the recorded 200
	if sw.status != 200 {
		t.Fatalf("status mutated after write: %d", sw.status)
	}
}

func TestRecoveryPreStreamReturns500(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	r := New(Options{})
	r.Use(Recovery(log))
	r.GET("/boom", func(c *Ctx) { panic("kaboom") })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body = %q, want the uniform error envelope", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "kaboom") {
		t.Fatalf("panic not logged: %s", buf.String())
	}
}

// TestRecoveryMidStreamKeepsPartialResponse: once an SSE handler has
// written and flushed, a panic must not attempt to inject a 500 into the
// stream — the partial response stands and the error is logged.
func TestRecoveryMidStreamKeepsPartialResponse(t *testing.T) {
	r := New(Options{})
	r.Use(Recovery(zerolog.Nop()))
	r.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_ = rc.Flush()
		_, _ = w.Write([]byte("data: first\n\n"))
		_ = rc.Flush()
		panic("mid-stream failure")
	}))
	rec := newSSERecorder()
	defer func() {
		if recoverErr := recover(); recoverErr != nil {
			t.Fatalf("panic escaped Recovery: %v", recoverErr)
		}
	}()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stream", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data: first") {
		t.Fatalf("partial response destroyed: %d %q", rec.Code, rec.Body.String())
	}
}

func TestRecoveryErrAbortHandlerRepanics(t *testing.T) {
	r := New(Options{})
	r.Use(Recovery(zerolog.Nop()))
	r.GET("/abort", func(c *Ctx) { panic(http.ErrAbortHandler) })
	defer func() {
		if recover() == nil {
			t.Fatal("http.ErrAbortHandler must be re-panicked for net/http to handle")
		}
	}()
	r.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/abort", nil))
}

func TestContextPlumbing(t *testing.T) {
	r := New(Options{})
	r.Use(RequestID())
	var seen string
	r.GET("/ctx", func(c *Ctx) { seen = RequestIDFrom(c.Request().Context()) })
	req := httptest.NewRequest("GET", "/ctx", nil)
	req.Header.Set("X-Request-Id", "trace-001")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if seen != "trace-001" {
		t.Fatalf("downstream context id = %q, want trace-001", seen)
	}
}

func TestGraceOptions(t *testing.T) {
	r := New(Options{})
	if r.grace() != defaultGrace {
		t.Fatalf("default grace = %v", r.grace())
	}
	r2 := New(Options{Grace: 40 * time.Second})
	if r2.grace() != 40*time.Second {
		t.Fatalf("custom grace = %v", r2.grace())
	}
	if r2.readHeaderTimeout() != defaultReadHeaderTimout {
		t.Fatalf("default read header timeout = %v", r2.readHeaderTimeout())
	}
	_ = context.Background()
}

// TestCaptureWriterReadFromFallbackNoRecursion: the fallback branch
// (underlying writer without ReaderFrom) must not recurse — io.Copy calls
// dst.ReadFrom when present, so copying onto the captureWriter itself
// used to overflow the stack (found live on the tunnel respond path).
func TestCaptureWriterReadFromFallbackNoRecursion(t *testing.T) {
	rec := httptest.NewRecorder() // implements WriteHeader/Write only
	sw := &captureWriter{ResponseWriter: rec, status: 200}
	body := strings.NewReader(" streamed body ")
	n, err := sw.ReadFrom(body)
	if err != nil || n != int64(len(" streamed body ")) {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if rec.Body.String() != " streamed body " {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if sw.bytes != n {
		t.Fatalf("bytes = %d, want %d", sw.bytes, n)
	}
}
