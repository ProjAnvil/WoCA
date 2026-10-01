package tracing

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/rs/zerolog"
)

func TestSpanCarriesOTelAttributes(t *testing.T) {
	rec := &recordingSink{}
	SetDefault(rec)
	t.Cleanup(func() { SetDefault(nil) })

	ctx := WithRun(tenancy.NewContext(context.Background(), tenancy.Context{
		TenantID: "t_acme", UserID: "u_alice",
	}), "run-1", "sess-1")
	sp := Start(ctx, "invoke_tool", KindTool)
	sp.Set(AttrToolName, "Bash")
	time.Sleep(time.Millisecond)
	sp.End(ctx, nil)

	if len(rec.spans) != 1 {
		t.Fatalf("recorded %d spans", len(rec.spans))
	}
	got := rec.spans[0]
	if got.Name != "invoke_tool" || got.Kind != KindTool || got.Status != StatusOK {
		t.Fatalf("span = %+v", got)
	}
	if got.Attributes[string(AttrToolName)] != "Bash" {
		t.Fatalf("attrs = %v", got.Attributes)
	}
	if got.DurationMS <= 0 {
		t.Fatalf("duration = %d", got.DurationMS)
	}
	for _, key := range []string{"tenant_id", "run_id", "session_id"} {
		if got.Tags[key] != "" && key == "run_id" && got.Tags[key] != "run-1" {
			t.Fatalf("tag %s = %q", key, got.Tags[key])
		}
	}
	if got.Tags["run_id"] != "run-1" || got.Tags["session_id"] != "sess-1" || got.Tags["tenant_id"] != "t_acme" {
		t.Fatalf("tags = %v", got.Tags)
	}
}

func TestSpanErrorStatus(t *testing.T) {
	rec := &recordingSink{}
	SetDefault(rec)
	t.Cleanup(func() { SetDefault(nil) })

	ctx := context.Background()
	sp := Start(ctx, "chat", KindLLM)
	sp.Set(AttrRequestModel, "glm-4.7")
	sp.End(ctx, context.DeadlineExceeded)

	got := rec.spans[0]
	if got.Status != StatusError || got.Error != "context deadline exceeded" {
		t.Fatalf("span = %+v", got)
	}
}

func TestLocalSinkWritesStructuredLog(t *testing.T) {
	var b strings.Builder
	log := zerolog.New(&b)
	sink := NewLocalSink(log)
	SetDefault(sink)
	t.Cleanup(func() { SetDefault(nil) })

	ctx := WithRun(context.Background(), "run-9", "sess-9")
	sp := Start(ctx, "chat", KindLLM)
	sp.Set(AttrRequestModel, "glm-4.7")
	sp.Set(AttrUsageOutputTokens, 42)
	sp.End(ctx, nil)

	line := b.String()
	for _, want := range []string{`"run_id":"run-9"`, `"gen_ai.request.model":"glm-4.7"`, `"gen_ai.usage.output_tokens":42`, `"telemetry.span":"chat"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
}

func TestNilSinkIsNoop(t *testing.T) {
	SetDefault(nil)
	ctx := context.Background()
	sp := Start(ctx, "x", KindTool)
	sp.End(ctx, nil) // must not panic
}

// recordingSink captures spans for assertions.
type recordingSink struct {
	mu    sync.Mutex
	spans []Span
}

func (r *recordingSink) Record(_ context.Context, sp Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, sp)
}

func TestMultiSinkFansOutAndSkipsNil(t *testing.T) {
	defer SetDefault(nil)
	a, b := &recordingSink{}, &recordingSink{}
	SetDefault(NewMultiSink(nil, a, b, nil))

	ctx := WithRun(context.Background(), "run-1", "sess-1")
	Start(ctx, "chat", KindLLM).End(ctx, nil)

	for name, s := range map[string]*recordingSink{"a": a, "b": b} {
		if len(s.spans) != 1 {
			t.Fatalf("sink %s recorded %d spans, want 1", name, len(s.spans))
		}
		if s.spans[0].Tags["run_id"] != "run-1" {
			t.Fatalf("sink %s missing run tag: %+v", name, s.spans[0].Tags)
		}
	}
}

func TestMultiSinkEmptyIsSafe(t *testing.T) {
	m := NewMultiSink()
	m.Record(context.Background(), Span{Name: "x"}) // must not panic
}
