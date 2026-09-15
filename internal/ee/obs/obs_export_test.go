package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/telemetry"
)

// TestSetupExportsTracesOverOTLP covers the live export path the unit tests
// with the in-memory recorder cannot see: spans recorded through Sink() must
// reach a real OTLP/HTTP collector endpoint (BatchSpanProcessor flush), the
// same wiring the worker/server roles use.
func TestSetupExportsTracesOverOTLP(t *testing.T) {
	var traces, metrics atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces":
			traces.Add(1)
		case "/v1/metrics":
			metrics.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, err := Setup(context.Background(), Config{
		ServiceName:  "probe",
		OTLPEndpoint: srv.URL,
		OTLPInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Shutdown(context.Background()) }()

	sink := o.Sink()
	if sink == nil {
		t.Fatal("Sink() is nil with an OTLP endpoint configured")
	}

	// Debug: is a span created through the sink's tracer even recorded and
	// sampled? (BatchSpanProcessor drops non-sampled spans silently.)
	tr := o.tracer
	_, span := tr.Start(context.Background(), "probe.span")
	t.Logf("recording=%v sampled=%v valid=%v", span.IsRecording(), span.SpanContext().IsSampled(), span.SpanContext().IsValid())
	span.End()

	ctx := telemetry.WithRun(context.Background(), "run-9", "sess-9")
	sp := telemetry.Start(ctx, "chat", telemetry.KindLLM)
	sp.Set(telemetry.AttrOperationName, "chat")
	sp.End(ctx, nil)

	// BatchSpanProcessor flushes on its 5s interval; Shutdown would flush
	// too, but the point is the periodic path.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && traces.Load() == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if traces.Load() == 0 {
		t.Fatalf("no trace export reached the collector (metrics pushes: %d)", metrics.Load())
	}
}

// TestSetupResolvesRegistryReference: a nacos:// OTLP endpoint resolves
// through the deployment-layer registry at startup (same source as WOCA_DB
// references); an unreachable registry fails Setup loudly instead of
// silently dropping export.
func TestSetupResolvesRegistryReference(t *testing.T) {
	// Unreachable registry: Setup must fail.
	if _, err := Setup(context.Background(), Config{
		ServiceName:  "probe",
		OTLPEndpoint: "nacos://127.0.0.1:1/DEFAULT_GROUP/woca-obs.otlp",
	}); err == nil {
		t.Fatal("unreachable registry reference must fail Setup")
	}

	// Fake nacos serving the collector address: Setup succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nacos/v1/cs/configs":
			_, _ = w.Write([]byte("http://collector.invalid:4318\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o, err := Setup(context.Background(), Config{
		ServiceName:  "probe",
		OTLPEndpoint: "nacos://" + srv.Listener.Addr().String() + "/DEFAULT_GROUP/woca-obs.otlp",
	})
	if err != nil {
		t.Fatalf("registry reference resolution: %v", err)
	}
	_ = o.Shutdown(context.Background())
}
