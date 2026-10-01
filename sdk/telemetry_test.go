package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/tracing"
)

type captureSink struct {
	mu    sync.Mutex
	spans []Span
}

func (c *captureSink) Record(_ context.Context, sp Span) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, sp)
}

func (c *captureSink) snapshot() []Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Span(nil), c.spans...)
}

func TestWithTelemetryReceivesModelSpan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { tracing.SetDefault(nil) })

	srv := fakeAnthropic(t)
	defer srv.Close()

	cap := &captureSink{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
		WithTelemetry(cap),
	)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}

	var chat *Span
	for i, sp := range cap.snapshot() {
		if sp.Kind == tracing.KindLLM {
			chat = &sp
			_ = i
			break
		}
	}
	if chat == nil {
		t.Fatal("no llm span captured by the injected sink")
	}
	if got := chat.Attributes["gen_ai.request.model"]; got != "claude-sonnet-4-5" {
		t.Fatalf("llm span model attribute = %v", got)
	}
	if chat.Tags["run_id"] == "" || chat.Tags["session_id"] == "" {
		t.Fatalf("llm span missing run/session tags: %+v", chat.Tags)
	}
}

// TestInstallTelemetryIdempotent: installing the same sink twice must not
// re-chain the process default (identity-deduped, so no double recording).
func TestInstallTelemetryIdempotent(t *testing.T) {
	t.Cleanup(func() { tracing.SetDefault(nil) })
	cap := &captureSink{}
	o := Options{Telemetry: []TelemetrySink{cap}}
	o.installTelemetry()
	first := tracing.Default()
	o.installTelemetry()
	if tracing.Default() != first {
		t.Fatal("second installTelemetry must be a no-op for an already-seen sink")
	}
	if tracing.Default() == nil {
		t.Fatal("first installTelemetry must install the sink")
	}
}
