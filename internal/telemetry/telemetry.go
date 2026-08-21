// Package telemetry is the Phase 2 observability seam (design §6.5): one
// span per skill load/activation, tool invocation, and LLM call, with
// attributes named after the OTel GenAI semantic conventions. The local
// sink writes structured log lines tagged with tenant/run/session so a
// run's full call chain reconstructs from logs plus the run_events stream
// (tool_call/tool_result frames are mirrored there by the server); an OTel
// exporter sink is the later remote implementation.
package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/rs/zerolog"
)

// Span kinds (map onto OTel GenAI operation names).
const (
	KindTool  = "tool"
	KindLLM   = "llm"
	KindSkill = "skill"
)

// Status values.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Attribute keys follow the OTel GenAI semantic conventions
// (opentelemetry.io/docs/specs/semconv/gen-ai/) so a future OTel sink maps
// them without translation.
const (
	AttrOperationName     = "gen_ai.operation.name"
	AttrSystemName        = "gen_ai.system.name"
	AttrRequestModel      = "gen_ai.request.model"
	AttrResponseModel     = "gen_ai.response.model"
	AttrUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens = "gen_ai.usage.output_tokens"
	AttrToolName          = "gen_ai.tool.name"
	AttrSkillName         = "woca.skill.name" // no GenAI convention yet
	AttrSkillVersion      = "woca.skill.version"
)

// Span is one recorded operation.
type Span struct {
	Name       string
	Kind       string
	Status     string
	Error      string
	Start      time.Time
	DurationMS int64
	Attributes map[string]any
	// Tags carry the correlation identity (tenant/run/session), separated
	// from GenAI attributes so exporters can route them as resources.
	Tags map[string]string
}

// Sink receives finished spans. Implementations must be safe for
// concurrent use.
type Sink interface {
	Record(ctx context.Context, sp Span)
}

var defaultSink atomic.Pointer[Sink]

// SetDefault installs the process-wide sink (nil disables recording).
func SetDefault(s Sink) {
	if s == nil {
		defaultSink.Store(nil)
		return
	}
	defaultSink.Store(&s)
}

// Default returns the installed sink (nil when disabled).
func Default() Sink {
	if p := defaultSink.Load(); p != nil {
		return *p
	}
	return nil
}

type runKey struct{}

// RunScope is the correlation identity for spans.
type RunScope struct {
	RunID     string
	SessionID string
}

// WithRun tags ctx with the run/session identity every span carries.
func WithRun(ctx context.Context, runID, sessionID string) context.Context {
	return context.WithValue(ctx, runKey{}, RunScope{RunID: runID, SessionID: sessionID})
}

// RunFrom returns the run scope in ctx, if any.
func RunFrom(ctx context.Context) (RunScope, bool) {
	sc, ok := ctx.Value(runKey{}).(RunScope)
	return sc, ok
}

// Start begins a span. The returned builder is finished by End; a nil
// default sink makes the whole call chain a no-op.
func Start(ctx context.Context, name, kind string) *Builder {
	return &Builder{ctx: ctx, name: name, kind: kind, start: time.Now()}
}

// Builder accumulates attributes and finishes one span.
type Builder struct {
	ctx   context.Context
	name  string
	kind  string
	start time.Time
	attrs map[string]any
}

// Set records one attribute.
func (b *Builder) Set(key string, value any) *Builder {
	if b.attrs == nil {
		b.attrs = map[string]any{}
	}
	b.attrs[key] = value
	return b
}

// End finishes the span: err != nil marks the error status. No sink → noop.
func (b *Builder) End(ctx context.Context, err error) {
	sink := Default()
	if sink == nil {
		return
	}
	sp := Span{
		Name:       b.name,
		Kind:       b.kind,
		Start:      b.start,
		DurationMS: time.Since(b.start).Milliseconds(),
		Attributes: b.attrs,
		Status:     StatusOK,
		Tags:       map[string]string{},
	}
	if err != nil {
		sp.Status = StatusError
		sp.Error = err.Error()
	}
	if tc, ok := tenancy.FromContext(ctx); ok {
		sp.Tags["tenant_id"] = tc.TenantID
		sp.Tags["user_id"] = tc.UserID
	}
	if sc, ok := RunFrom(ctx); ok {
		sp.Tags["run_id"] = sc.RunID
		sp.Tags["session_id"] = sc.SessionID
	}
	sink.Record(ctx, sp)
}

// MultiSink fans every span out to the wrapped sinks in order. The server
// editions use it to chain the local structured-log sink with the OTel
// exporter sink (design §6.5.3: remote = OTel export *plus* tagged
// run_events/logs, not instead of them). A nil sink in the list is skipped.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink returns a sink broadcasting to the non-nil sinks.
func NewMultiSink(sinks ...Sink) *MultiSink {
	kept := make([]Sink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &MultiSink{sinks: kept}
}

// Record implements Sink.
func (m *MultiSink) Record(ctx context.Context, sp Span) {
	for _, s := range m.sinks {
		s.Record(ctx, sp)
	}
}

// LocalSink writes one structured log line per span.
type LocalSink struct {
	log zerolog.Logger
}

// NewLocalSink returns the structured-log sink.
func NewLocalSink(log zerolog.Logger) *LocalSink {
	return &LocalSink{log: log}
}

// Record implements Sink.
func (s *LocalSink) Record(_ context.Context, sp Span) {
	evt := s.log.Info()
	if sp.Status == StatusError {
		evt = s.log.Error().Str("error", sp.Error)
	}
	evt = evt.Str("telemetry.span", sp.Name).
		Str("telemetry.kind", sp.Kind).
		Str("telemetry.status", sp.Status).
		Int64("telemetry.duration_ms", sp.DurationMS)
	for k, v := range sp.Tags {
		evt = evt.Str(k, v)
	}
	for k, v := range sp.Attributes {
		evt = evt.Interface(k, v)
	}
	evt.Send()
}
