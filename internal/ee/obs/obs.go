// Package obs is the enterprise observability assembly (design §12.3):
// OpenTelemetry SDK providers with OTLP export, a Prometheus /metrics
// endpoint, RED metrics for every HTTP route, /healthz for probes, and a
// telemetry sink mapping woca spans onto the OTel GenAI attributes.
//
// It is linked only by the server artifacts (woca-server, woca-worker).
// The personal edition keeps the zero-dependency local sink (§5.2), so the
// dependency-hygiene baseline never sees OTel packages.
package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otlpmexp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otlptexp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"

	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/telemetry"
)

// Environment contract (design §12.2: infrastructure config rides env /
// ConfigMap, never the tenant configuration platform):
//
//	WOCA_SERVICE_NAME                        service.name resource attribute
//	                                        (default: the binary name)
//	WOCA_OTEL_EXPORTER_OTLP_ENDPOINT         OTLP collector, e.g.
//	                                        http://otel-collector:4318 — or a
//	                                        deployment-layer registry
//	                                        reference (nacos://nacos:8848/
//	                                        DEFAULT_GROUP/woca-obs.otlp),
//	                                        resolved once at startup like
//	                                        WOCA_DB (falls back to OTel's
//	                                        standard OTEL_EXPORTER_OTLP_ENDPOINT)
//	WOCA_OTEL_EXPORTER_OTLP_INSECURE         "false" to use TLS; plain HTTP
//	                                        is the in-cluster default
//
// Delivery contract: export is fully asynchronous and bounded — recording a
// span enqueues into an in-memory batch queue (microseconds, no network on
// the request path); a background goroutine flushes every 5s; a full queue
// drops spans (never blocks the caller); an unreachable collector retries
// with backoff and never affects the process. Metrics push the same way on
// a 15s period.
const (
	envServiceName  = "WOCA_SERVICE_NAME"
	envOTLPEndpoint = "WOCA_OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPInsecure = "WOCA_OTEL_EXPORTER_OTLP_INSECURE"
)

const scopeName = "github.com/projanvil/woca/internal/ee/obs"

// Config wires the observability stack.
type Config struct {
	ServiceName  string
	OTLPEndpoint string // empty = no OTLP export (metrics still serve /metrics)
	OTLPInsecure bool
	// OTLPTokenProvider, when set, supplies a bearer credential on every
	// OTLP export — the bridge form's machine token, which is loaded
	// after startup and rotates on renewal, so it cannot ride a static
	// exporter header.
	OTLPTokenProvider func() string
}

// FromEnv resolves Config from the environment.
func FromEnv() Config {
	cfg := Config{
		ServiceName:  os.Getenv(envServiceName),
		OTLPEndpoint: os.Getenv(envOTLPEndpoint),
		OTLPInsecure: true,
	}
	if cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if v := os.Getenv(envOTLPInsecure); v != "" {
		cfg.OTLPInsecure = !(strings.EqualFold(v, "false") || v == "0")
	}
	if cfg.ServiceName == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.ServiceName = filepath.Base(exe)
		} else {
			cfg.ServiceName = "woca"
		}
	}
	return cfg
}

// Obs owns the OTel providers and the HTTP observability handlers.
type Obs struct {
	serviceName string
	tracer      trace.Tracer

	reg  *prometheus.Registry
	shut []func(context.Context) error
	mu   sync.Mutex

	reqCount  metric.Int64Counter
	reqDur    metric.Float64Histogram
	activeReq metric.Int64UpDownCounter

	workerInit    func()
	workerClaimed metric.Int64Counter
	workerRuns    metric.Int64Counter
	workerRunDur  metric.Float64Histogram

	// svcActive/svcQueued back the always-present service run-state
	// gauges (observable callbacks — the series exist from boot, unlike
	// lazy counters which need a first record).
	svcActive atomic.Int64
	svcQueued atomic.Int64
}

// Setup initializes the global OTel providers and returns the Obs handle.
// The process telemetry default sink is left alone; callers chain Sink()
// themselves so the local log sink stays installed (§6.5.3: remote export
// rides on top of local logging, not instead of it).
func Setup(ctx context.Context, cfg Config) (*Obs, error) {
	if ref, err := discovery.Resolve(ctx, cfg.OTLPEndpoint); err != nil {
		// A registry reference that cannot be resolved is an assembly
		// failure — fail loudly, same posture as a WOCA_DB reference that
		// does not resolve (plain endpoints pass through untouched).
		return nil, fmt.Errorf("obs: resolve OTLP endpoint: %w", err)
	} else {
		cfg.OTLPEndpoint = ref
	}
	o := &Obs{serviceName: cfg.ServiceName, reg: prometheus.NewRegistry()}
	o.workerInit = sync.OnceFunc(func() {
		meter := otel.GetMeterProvider().Meter(scopeName)
		o.workerClaimed, _ = meter.Int64Counter("woca.worker.claimed")
		o.workerRuns, _ = meter.Int64Counter("woca.worker.runs")
		o.workerRunDur, _ = meter.Float64Histogram("woca.worker.run.duration", metric.WithUnit("s"))
	})

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("obs: resource: %w", err)
	}

	// Metrics: Prometheus exposition (always) + OTLP push (when configured).
	prom, err := otelprom.New(
		otelprom.WithRegisterer(o.reg),
		//nolint:staticcheck // SA1019: no TranslationStrategy reproduces "unit
		// suffixes but no _total" — WithoutSuffixes also drops unit suffixes
		// (e.g. woca_worker_run_duration_seconds), WithSuffixes adds _total.
		otelprom.WithoutCounterSuffixes(),
	)
	if err != nil {
		return nil, fmt.Errorf("obs: prometheus exporter: %w", err)
	}
	mopts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(prom),
	}
	if cfg.OTLPEndpoint != "" {
		exp, err := otlpMetricExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}
		reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(15*time.Second))
		mopts = append(mopts, sdkmetric.WithReader(reader))
		o.shutdown(func(c context.Context) error { return reader.Shutdown(c) })
	}
	mp := sdkmetric.NewMeterProvider(mopts...)
	otel.SetMeterProvider(mp)
	o.shutdown(func(c context.Context) error { return mp.Shutdown(c) })
	if err := otelruntime.Start(otelruntime.WithMeterProvider(mp)); err != nil {
		return nil, fmt.Errorf("obs: runtime metrics: %w", err)
	}
	meter := mp.Meter(scopeName)

	// Traces: OTLP push when configured; a processor-less provider otherwise
	// keeps the global API satisfied and spans cheap.
	topts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if cfg.OTLPEndpoint != "" {
		exp, err := otlpTraceExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}
		// Async + bounded by design (delivery contract above): the batch
		// queue never blocks the caller — overflow drops, the flush and
		// the HTTP round-trips run on a background goroutine.
		bsp := sdktrace.NewBatchSpanProcessor(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		)
		topts = append(topts, sdktrace.WithSpanProcessor(bsp))
		o.shutdown(func(c context.Context) error { return bsp.Shutdown(c) })
	}
	tp := sdktrace.NewTracerProvider(topts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	o.shutdown(func(c context.Context) error { return tp.Shutdown(c) })
	o.tracer = tp.Tracer(scopeName)

	// HTTP RED instruments (attribute keys follow the OTel HTTP semconv).
	// Service run-state gauges (design §12.3): active and queued runs,
	// reported by the role (servers from their admission state). Observable
	// callbacks keep the series present from boot.
	if _, err = meter.Int64ObservableGauge("woca.service.active_runs",
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(o.svcActive.Load())
			return nil
		})); err != nil {
		return nil, fmt.Errorf("obs: active_runs gauge: %w", err)
	}
	if _, err = meter.Int64ObservableGauge("woca.service.queued_runs",
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(o.svcQueued.Load())
			return nil
		})); err != nil {
		return nil, fmt.Errorf("obs: queued_runs gauge: %w", err)
	}

	if o.reqCount, err = meter.Int64Counter("http.server.request.count"); err != nil {
		return nil, fmt.Errorf("obs: request counter: %w", err)
	}
	if o.reqDur, err = meter.Float64Histogram("http.server.request.duration",
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("obs: duration histogram: %w", err)
	}
	if o.activeReq, err = meter.Int64UpDownCounter("http.server.active_requests"); err != nil {
		return nil, fmt.Errorf("obs: active gauge: %w", err)
	}
	return o, nil
}

// otlpTokenTransport injects a bearer credential on every OTLP export.
type otlpTokenTransport struct {
	next  http.RoundTripper
	token func() string
}

func (t otlpTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if tok := t.token(); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// tokenClient wraps the default transport with the configured token
// provider (nil provider → nil, meaning the SDK's own client).
func tokenClient(provider func() string) *http.Client {
	if provider == nil {
		return nil
	}
	return &http.Client{Transport: otlpTokenTransport{token: provider}}
}

// otlpTraceExporter builds the OTLP/HTTP trace exporter for cfg.
func otlpTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	host, path, insecure := splitEndpoint(cfg, "/v1/traces")
	opts := []otlptexp.Option{otlptexp.WithEndpoint(host), otlptexp.WithURLPath(path)}
	if insecure {
		opts = append(opts, otlptexp.WithInsecure())
	}
	if c := tokenClient(cfg.OTLPTokenProvider); c != nil {
		opts = append(opts, otlptexp.WithHTTPClient(c))
	}
	exp, err := otlptexp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("obs: otlp trace exporter: %w", err)
	}
	return exp, nil
}

// otlpMetricExporter builds the OTLP/HTTP metric exporter for cfg.
func otlpMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	host, path, insecure := splitEndpoint(cfg, "/v1/metrics")
	opts := []otlpmexp.Option{otlpmexp.WithEndpoint(host), otlpmexp.WithURLPath(path)}
	if insecure {
		opts = append(opts, otlpmexp.WithInsecure())
	}
	if c := tokenClient(cfg.OTLPTokenProvider); c != nil {
		opts = append(opts, otlpmexp.WithHTTPClient(c))
	}
	exp, err := otlpmexp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("obs: otlp metric exporter: %w", err)
	}
	return exp, nil
}

// splitEndpoint normalizes the configured OTLP endpoint into host:port and
// the full request path (an endpoint path prefix — e.g. the edge's
// /gw/obs machine channel — is preserved ahead of the signal path).
func splitEndpoint(cfg Config, signalPath string) (host, fullPath string, insecure bool) {
	host = cfg.OTLPEndpoint
	prefix := ""
	insecure = cfg.OTLPInsecure
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
		prefix = strings.TrimSuffix(u.Path, "/")
		if u.Scheme == "https" {
			insecure = false
		}
	}
	if !strings.Contains(host, ":") {
		host += ":4318"
	}
	return host, prefix + signalPath, insecure
}

func (o *Obs) shutdown(fn func(context.Context) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.shut = append(o.shut, fn)
}

// Shutdown flushes and stops all providers (reverse registration order).
func (o *Obs) Shutdown(ctx context.Context) error {
	o.mu.Lock()
	fns := append([]func(context.Context) error(nil), o.shut...)
	o.mu.Unlock()
	var errs []error
	for i := len(fns) - 1; i >= 0; i-- {
		if err := fns[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("obs: shutdown: %w", errors.Join(errs...))
	}
	return nil
}

// ServiceName reports the configured service name.
func (o *Obs) ServiceName() string { return o.serviceName }

// Wrap serves /healthz and /metrics at the root of the returned handler and
// records RED metrics for everything else. Attach it at the outermost edge
// of an HTTP handler chain so probe requests bypass auth (infrastructure
// liveness, same exemption as /api/health).
func (o *Obs) Wrap(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", http.HandlerFunc(o.handleHealthz))
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.reg, promhttp.HandlerOpts{}))
	mux.Handle("/", o.red(h))
	return mux
}

// Handler returns just the observability endpoints — for roles without a
// business HTTP surface (the worker) to serve on a dedicated probe port.
func (o *Obs) Handler() http.Handler {
	return o.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no business surface on this port", http.StatusNotFound)
	}))
}

func (o *Obs) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": o.serviceName})
}

// red records per-route request rate / errors / duration around h. The
// route label collapses to the first two path segments: the mux matches on
// full patterns but does not expose them, and two segments separate every
// woca route family (/api/runs, /api/sessions, ...) without leaking
// per-resource cardinality.
func (o *Obs) red(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routeLabel(r.URL.Path)
		base := []attribute.KeyValue{
			attribute.String("http.route", route),
			attribute.String("http.request.method", r.Method),
		}
		o.activeReq.Add(r.Context(), 1, metric.WithAttributes(base...))
		defer o.activeReq.Add(r.Context(), -1, metric.WithAttributes(base...))

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)

		attrs := append(base, attribute.Int("http.response.status_code", sw.status))
		o.reqCount.Add(r.Context(), 1, metric.WithAttributes(attrs...))
		o.reqDur.Record(r.Context(), time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	})
}

// routeLabel collapses a request path for the RED dimension. Business
// routes keep their family (/api/runs, /api/sessions, ...); everything else
// — SPA assets with content-hashed names, the skills console — reduces to
// its first segment so the label cardinality stays a fixed handful.
func routeLabel(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	var out []string
	for _, s := range segs {
		if s != "" {
			out = append(out, s)
			break
		}
	}
	if len(out) == 0 {
		return "/"
	}
	if out[0] == "api" {
		for _, s := range segs[1:] {
			if s != "" {
				return "/api/" + s
			}
		}
	}
	return "/" + out[0]
}

// statusWriter captures the response status code while staying transparent
// for flushing (SSE) and hijacking via Unwrap.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush,
// Hijack), which SSE handlers rely on.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Sink returns the OTel trace sink (nil-safe to chain). Spans carry the
// GenAI attributes recorded by internal/telemetry plus the tenant/run tags
// as plain span attributes.
func (o *Obs) Sink() telemetry.Sink {
	if o.tracer == nil {
		return nil
	}
	return &otelSink{tracer: o.tracer}
}

type otelSink struct{ tracer trace.Tracer }

// Record implements telemetry.Sink.
func (s *otelSink) Record(ctx context.Context, sp telemetry.Span) {
	attrs := make([]attribute.KeyValue, 0, len(sp.Attributes)+len(sp.Tags))
	for k, v := range sp.Tags {
		attrs = append(attrs, attribute.String(k, v))
	}
	for k, v := range sp.Attributes {
		attrs = append(attrs, attrValue(k, v))
	}
	opts := []trace.SpanStartOption{
		trace.WithTimestamp(sp.Start),
		trace.WithAttributes(attrs...),
	}
	_, span := s.tracer.Start(ctx, sp.Name, opts...)
	if sp.Status == telemetry.StatusError {
		span.SetStatus(codes.Error, sp.Error)
		span.RecordError(errors.New(sp.Error))
	}
	span.End(trace.WithTimestamp(sp.Start.Add(time.Duration(sp.DurationMS) * time.Millisecond)))
}

// attrValue maps a telemetry attribute value (numbers, strings, bools)
// onto the typed OTel attribute constructors.
func attrValue(k string, v any) attribute.KeyValue {
	switch t := v.(type) {
	case string:
		return attribute.String(k, t)
	case bool:
		return attribute.Bool(k, t)
	case int:
		return attribute.Int(k, t)
	case int32:
		return attribute.Int64(k, int64(t))
	case int64:
		return attribute.Int64(k, t)
	case float32:
		return attribute.Float64(k, float64(t))
	case float64:
		return attribute.Float64(k, t)
	default:
		return attribute.String(k, fmt.Sprintf("%v", t))
	}
}

// WorkerMetrics are the worker-role instruments (design §12.3: queue
// consumption visibility). Instruments are created lazily so roles that
// never touch them pay nothing.
type WorkerMetrics struct {
	Claimed metric.Int64Counter
	Runs    metric.Int64Counter
	RunDur  metric.Float64Histogram
}

// SetRunState updates the service run-state gauges (active and queued
// runs). Roles push into it; the observable gauges export the values.
func (o *Obs) SetRunState(active, queued int64) {
	o.svcActive.Store(active)
	o.svcQueued.Store(queued)
}

// Worker returns the worker instruments, bound to the meter provider the
// Obs was set up with.
func (o *Obs) Worker() *WorkerMetrics {
	o.workerInit()
	if o.workerClaimed == nil {
		return nil
	}
	return &WorkerMetrics{Claimed: o.workerClaimed, Runs: o.workerRuns, RunDur: o.workerRunDur}
}

// Install implements the servefront.Observability seam: it chains the OTel
// sink behind whatever sink is currently the telemetry default, so the
// enterprise deployments keep local structured logs first and add OTel export on
// top (design §6.5.3). It assumes the local sink was already installed by
// the caller.
func (o *Obs) Install(_ context.Context) error {
	if s := o.Sink(); s != nil {
		telemetry.SetDefault(telemetry.NewMultiSink(telemetry.Default(), s))
	}
	return nil
}
