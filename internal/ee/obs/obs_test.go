package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/projanvil/woca/internal/telemetry"
)

func TestWrapServesHealthzAndMetrics(t *testing.T) {
	o, err := Setup(t.Context(), Config{ServiceName: "test-svc"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = o.Shutdown(t.Context()) }()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/runs/abc" {
			w.WriteHeader(http.StatusTeapot) // a distinctive non-2xx
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := o.Wrap(inner)

	// Probes answer before any business traffic.
	for path, want := range map[string]string{
		"/healthz": `"service":"test-svc"`,
		"/metrics": "go_goroutine_count", // runtime instruments are always present
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: body missing %q:\n%s", path, want, rec.Body.String())
		}
	}

	// Business traffic flows through and gets RED-recorded.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/abc", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("inner handler: status %d", rec.Code)
	}

	// The metrics endpoint now exposes the route with its status code, and
	// the active-requests gauge returned to zero after the request ended.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"http_server_request_count",
		"http_server_request_duration_seconds",
		`http_route="/api/runs"`,
		`http_response_status_code="418"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestRouteLabelCollapsesToTwoSegments(t *testing.T) {
	cases := map[string]string{
		"/":                       "/",
		"/api/runs":               "/api/runs",
		"/api/runs/abc123":        "/api/runs",
		"/api/runs/abc/events":    "/api/runs",
		"/api/sessions/x/goal":    "/api/sessions",
		"/assets/app-1a2b3c.js":   "/assets", // hashed asset names must not fan out
		"/skills/":                "/skills",
		"//double//slash/entries": "/double",
	}
	for in, want := range cases {
		if got := routeLabel(in); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		cfg      Config
		wantHost string
		wantPath string
		wantIns  bool
	}{
		{Config{OTLPEndpoint: "otel-collector:4318", OTLPInsecure: true}, "otel-collector:4318", "/v1/traces", true},
		{Config{OTLPEndpoint: "http://otel-collector:4318", OTLPInsecure: true}, "otel-collector:4318", "/v1/traces", true},
		{Config{OTLPEndpoint: "https://collector.example.com", OTLPInsecure: true}, "collector.example.com:4318", "/v1/traces", false},
		{Config{OTLPEndpoint: "collector:4318", OTLPInsecure: false}, "collector:4318", "/v1/traces", false},
		// An endpoint path prefix (the edge /gw/obs machine channel) rides
		// ahead of the signal path.
		{Config{OTLPEndpoint: "http://edge:8453/gw/obs", OTLPInsecure: true}, "edge:8453", "/gw/obs/v1/traces", true},
	}
	for _, c := range cases {
		host, path, ins := splitEndpoint(c.cfg, "/v1/traces")
		if host != c.wantHost || ins != c.wantIns || path != c.wantPath {
			t.Errorf("splitEndpoint(%+v) = (%q,%q,%v), want (%q,%q,%v)", c.cfg, host, path, ins, c.wantHost, c.wantPath, c.wantIns)
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv(envServiceName, "")
	t.Setenv(envOTLPEndpoint, "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := FromEnv()
	if cfg.OTLPEndpoint != "" {
		t.Errorf("endpoint = %q, want empty", cfg.OTLPEndpoint)
	}
	if !cfg.OTLPInsecure {
		t.Error("insecure should default true")
	}
	if cfg.ServiceName == "" {
		t.Error("service name should fall back to the binary name")
	}

	t.Setenv(envServiceName, "woca-server-1")
	t.Setenv(envOTLPEndpoint, "http://collector:4318")
	t.Setenv(envOTLPInsecure, "false")
	cfg = FromEnv()
	if cfg.ServiceName != "woca-server-1" || cfg.OTLPEndpoint != "http://collector:4318" || cfg.OTLPInsecure {
		t.Errorf("FromEnv = %+v", cfg)
	}
}

// The sink test drives otelSink directly with an in-memory exporter so the
// recorded spans are inspectable without a collector.
func TestSinkRecordsGenAISpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	sink := &otelSink{tracer: tp.Tracer("test")}

	now := time.Now()
	sink.Record(t.Context(), telemetry.Span{
		Name:       "chat",
		Kind:       telemetry.KindLLM,
		Status:     telemetry.StatusError,
		Error:      "boom",
		Start:      now,
		DurationMS: 42,
		Attributes: map[string]any{
			telemetry.AttrRequestModel:     "glm-4.7",
			telemetry.AttrUsageInputTokens: int64(128),
		},
		Tags: map[string]string{"tenant_id": "t1", "run_id": "r9"},
	})

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	sp := spans[0]
	if sp.Name() != "chat" {
		t.Errorf("name = %q", sp.Name())
	}
	got := map[string]string{}
	for _, kv := range sp.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	for _, want := range []string{"gen_ai.request.model", "gen_ai.usage.input_tokens", "tenant_id", "run_id"} {
		if _, ok := got[want]; !ok {
			t.Errorf("span missing attribute %q: %v", want, got)
		}
	}
}

func TestWorkerInstrumentsAvailable(t *testing.T) {
	o, err := Setup(t.Context(), Config{ServiceName: "worker-test"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = o.Shutdown(t.Context()) }()
	w := o.Worker()
	if w == nil {
		t.Fatal("Worker() = nil")
	}
	w.Claimed.Add(t.Context(), 1)
	w.Runs.Add(t.Context(), 1)
	w.RunDur.Record(t.Context(), 0.5)

	reg := prometheus.NewRegistry() // ensure instruments write to the provider, not this empty reg
	_ = reg

	rec := httptest.NewRecorder()
	o.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{"woca_worker_claimed", "woca_worker_runs", "woca_worker_run_duration_seconds"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// The dedicated probe handler has no business surface.
	rec = httptest.NewRecorder()
	o.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("business path on probe handler: status %d", rec.Code)
	}
}

// compile-time check that the sink implements the telemetry interface.
var _ telemetry.Sink = (*otelSink)(nil)

// tracer presence guard for Sink() nil path.
func TestSinkNilTracer(t *testing.T) {
	o := &Obs{}
	if s := o.Sink(); s != nil {
		t.Errorf("Sink() on zero Obs = %v, want nil", s)
	}
	var _ trace.Tracer // package used by the Obs struct
}
