package obs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/rs/zerolog"
)

// spanLog captures the local sink's span lines for assertions.
func spanLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := tracing.Default()
	tracing.SetDefault(tracing.NewLocalSink(zerolog.New(buf)))
	return buf, func() { tracing.SetDefault(prev) }
}

func TestHTTPMiddlewareSpanAttributes(t *testing.T) {
	buf, restore := spanLog(t)
	defer restore()

	o, err := Setup(t.Context(), Config{ServiceName: "woca-server"})
	if err != nil {
		t.Fatal(err)
	}

	r := webfx.New(webfx.Options{})
	r.Use(webfx.RequestID(), o.HTTPMiddleware())
	r.GET("/api/sessions/{id}", func(c *webfx.Ctx) { c.JSON(200, map[string]string{"ok": "1"}) })
	r.GET("/api/boom", func(c *webfx.Ctx) { c.Error(500, "x") })

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.Header.Set("X-Request-Id", "trace-42")
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	for _, want := range []string{
		`"telemetry.span":"http"`,
		`"telemetry.kind":"server"`,
		`"http.route":"/api/sessions/{id}"`, // pattern, not the raw path
		`"http.request.method":"GET"`,
		`"http.response.status_code":200`,
		`"service.name":"woca-server"`,
		`"woca.http.request_id":"trace-42"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("span line missing %s:\n%s", want, line)
		}
	}
	if strings.Contains(line, "/api/sessions/s1") {
		t.Errorf("span leaked the raw resource path (cardinality):\n%s", line)
	}
}

func TestHTTPMiddlewareTenantAndErrorStatus(t *testing.T) {
	buf, restore := spanLog(t)
	defer restore()

	o, err := Setup(t.Context(), Config{ServiceName: "woca-ops"})
	if err != nil {
		t.Fatal(err)
	}
	r := webfx.New(webfx.Options{})
	r.Use(o.HTTPMiddleware())
	r.GET("/api/x", func(c *webfx.Ctx) { c.Error(503, "unavailable") })

	req := httptest.NewRequest("GET", "/api/x", nil)
	req = req.WithContext(tenancy.NewContext(req.Context(), tenancy.Context{TenantID: "t9"}))
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if !strings.Contains(line, `"woca.tenant_id":"t9"`) {
		t.Errorf("tenant missing from span:\n%s", line)
	}
	if !strings.Contains(line, `"telemetry.status":"error"`) {
		t.Errorf("5xx should mark the span errored:\n%s", line)
	}
}

func TestHTTPMiddlewareRequestIDFromResponseHeader(t *testing.T) {
	// Servefront mounts the span middleware OUTSIDE the server core's
	// RequestID; the id is then only on the echoed response header.
	buf, restore := spanLog(t)
	defer restore()

	o, err := Setup(t.Context(), Config{ServiceName: "woca-edge"})
	if err != nil {
		t.Fatal(err)
	}
	inner := webfx.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
		webfx.RequestID(),
	)
	h := o.HTTPMiddleware()(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/tunnel/poll", nil))

	if !strings.Contains(buf.String(), `"woca.http.request_id"`) {
		t.Errorf("request id not captured from the echoed header:\n%s", buf.String())
	}
}

func TestOTLPTokenTransport(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ts.Close()

	tr := otlpTokenTransport{token: func() string { return "machine-token-1" }}
	req, _ := http.NewRequest("POST", ts.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "Bearer machine-token-1" {
		t.Fatalf("Authorization = %q", got)
	}

	// Rotation: the provider is consulted per export, never cached.
	tr = otlpTokenTransport{token: func() string { return "rotated-2" }}
	req, _ = http.NewRequest("POST", ts.URL, nil)
	resp, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "Bearer rotated-2" {
		t.Fatalf("rotated Authorization = %q", got)
	}

	// Empty token: no header (the unenrolled posture exports bare — the
	// edge will refuse, by design).
	tr = otlpTokenTransport{token: func() string { return "" }}
	req, _ = http.NewRequest("POST", ts.URL, nil)
	req.Header.Set("Authorization", "")
	resp, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "" {
		t.Fatalf("empty token should send no Authorization, got %q", got)
	}
}
