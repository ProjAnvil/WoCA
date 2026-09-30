package sdk

import (
	"context"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/telemetry"
)

// Span is one recorded operation, the public mirror of the harness span.
// Attribute keys follow the OTel GenAI semantic conventions (gen_ai.*), so
// OTel-native backends (Langfuse, Phoenix, ...) map them without
// translation. Maps are shared with the recorder — treat them as read-only.
type Span struct {
	Name       string
	Kind       string // "llm" | "tool" | "skill" | "http" | "server"
	Status     string // "ok" | "error"
	Error      string
	Start      time.Time
	DurationMS int64
	Attributes map[string]any
	Tags       map[string]string // correlation identity: run_id, session_id, tenant_id, user_id
}

// TelemetrySink receives finished spans. Implementations must be safe for
// concurrent use. Sinks are process-wide: installing one routes every span
// the harness records (LLM calls, tool invocations, skill loads) to it, in
// addition to whatever sink is already installed.
type TelemetrySink interface {
	Record(ctx context.Context, sp Span)
}

// sinkAdapter adapts a public TelemetrySink onto the internal Sink.
type sinkAdapter struct{ sink TelemetrySink }

func (a sinkAdapter) Record(ctx context.Context, sp telemetry.Span) {
	a.sink.Record(ctx, Span{
		Name:       sp.Name,
		Kind:       sp.Kind,
		Status:     sp.Status,
		Error:      sp.Error,
		Start:      sp.Start,
		DurationMS: sp.DurationMS,
		Attributes: sp.Attributes,
		Tags:       sp.Tags,
	})
}

var (
	telemetryMu   sync.Mutex
	telemetrySeen = map[TelemetrySink]struct{}{}
)

// installTelemetry chains every not-yet-seen sink onto the process-wide
// telemetry default. Telemetry is process-global by design: the chain only
// grows (existing sinks stay in place, sinks are deduped by identity), so
// it is safe to pass the same sink to every Query. A host that owns a
// stateful sink (e.g. the Langfuse exporter's buffered queue) keeps its own
// reference and closes that directly — sessions never tear sinks down.
func (o Options) installTelemetry() {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	var fresh []telemetry.Sink
	for _, s := range o.Telemetry {
		if s == nil {
			continue
		}
		if _, ok := telemetrySeen[s]; ok {
			continue
		}
		telemetrySeen[s] = struct{}{}
		fresh = append(fresh, sinkAdapter{sink: s})
	}
	if len(fresh) > 0 {
		chain := append([]telemetry.Sink{telemetry.Default()}, fresh...)
		telemetry.SetDefault(telemetry.NewMultiSink(chain...))
	}
}
