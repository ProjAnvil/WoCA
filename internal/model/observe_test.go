package model

import (
	"context"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/ratelimiters"

	"github.com/projanvil/woca/internal/telemetry"
)

type recordingSink struct {
	spans []telemetry.Span
}

func (r *recordingSink) Record(_ context.Context, sp telemetry.Span) {
	r.spans = append(r.spans, sp)
}

func installSink(t *testing.T) *recordingSink {
	t.Helper()
	rec := &recordingSink{}
	prev := telemetry.Default()
	telemetry.SetDefault(rec)
	t.Cleanup(func() { telemetry.SetDefault(prev) })
	return rec
}

func TestObserveInvokeRecordsChatSpan(t *testing.T) {
	rec := installSink(t)

	resp := messages.AI("hello")
	resp.UsageMetadata = messages.UsageMetadata{InputTokens: 12, OutputTokens: 34, TotalTokens: 46}
	inner := language.NewFakeChatModel(language.WithResponses(resp))
	m := Observe(inner)

	ctx := telemetry.WithRun(context.Background(), "run-1", "sess-1")
	got, err := m.Invoke(ctx, []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got.Content != "hello" {
		t.Fatalf("content = %q", got.Content)
	}

	if len(rec.spans) != 1 {
		t.Fatalf("recorded %d spans", len(rec.spans))
	}
	sp := rec.spans[0]
	if sp.Name != "chat" || sp.Kind != telemetry.KindLLM || sp.Status != telemetry.StatusOK {
		t.Fatalf("span = %+v", sp)
	}
	if sp.Attributes[string(telemetry.AttrOperationName)] != "chat" {
		t.Fatalf("operation attr = %v", sp.Attributes)
	}
	if in, ok := sp.Attributes[string(telemetry.AttrUsageInputTokens)].(int); !ok || in != 12 {
		t.Fatalf("input tokens = %v", sp.Attributes[string(telemetry.AttrUsageInputTokens)])
	}
	if out, ok := sp.Attributes[string(telemetry.AttrUsageOutputTokens)].(int); !ok || out != 34 {
		t.Fatalf("output tokens = %v", sp.Attributes[string(telemetry.AttrUsageOutputTokens)])
	}
	// The run/session tags ride the ctx: downstream grouping keys on them.
	if sp.Tags["run_id"] != "run-1" || sp.Tags["session_id"] != "sess-1" {
		t.Fatalf("tags = %v", sp.Tags)
	}
}

func TestObserveInvokeErrorStatus(t *testing.T) {
	rec := installSink(t)

	inner := language.NewFakeChatModel(language.WithRateLimiter(failingLimiter{}))
	m := Observe(inner)

	if _, err := m.Invoke(context.Background(), []messages.Message{messages.Human("hi")}); err == nil {
		t.Fatal("expected invoke error")
	}
	if len(rec.spans) != 1 {
		t.Fatalf("recorded %d spans", len(rec.spans))
	}
	if rec.spans[0].Status != telemetry.StatusError {
		t.Fatalf("status = %v", rec.spans[0].Status)
	}
}

func TestObserveStreamEndsSpanWithUsage(t *testing.T) {
	rec := installSink(t)

	final := messages.AI("hel")
	final.UsageMetadata = messages.UsageMetadata{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	inner := language.NewFakeChatModel(language.WithStreamChunks(
		messages.AI("h"), final,
	))
	m := Observe(inner)

	st, err := m.Stream(context.Background(), []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, ok, err := st.Next(context.Background())
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(rec.spans) != 1 {
		t.Fatalf("recorded %d spans", len(rec.spans))
	}
	sp := rec.spans[0]
	if sp.Name != "chat" {
		t.Fatalf("span = %+v", sp)
	}
	if in, ok := sp.Attributes[string(telemetry.AttrUsageInputTokens)].(int); !ok || in != 5 {
		t.Fatalf("input tokens = %v", sp.Attributes[string(telemetry.AttrUsageInputTokens)])
	}
	if out, ok := sp.Attributes[string(telemetry.AttrUsageOutputTokens)].(int); !ok || out != 7 {
		t.Fatalf("output tokens = %v", sp.Attributes[string(telemetry.AttrUsageOutputTokens)])
	}
}

// The wrapper must stay transparent for the capability side channels: Name
// keys the prompt cache, Unwrap lets token counting reach the inner model.
func TestObservePassesNameAndUnwrapThrough(t *testing.T) {
	inner := language.NewFakeChatModel()
	m := Observe(inner)

	n, ok := m.(interface{ Name() string })
	if !ok || n.Name() == "" {
		t.Fatalf("Name() = %v, %v", n, ok)
	}
	u, ok := m.(interface{ Unwrap() language.ChatModel })
	if !ok || u.Unwrap() == nil {
		t.Fatal("Unwrap not forwarded")
	}
}

func TestObserveBindToolsRewraps(t *testing.T) {
	inner := language.NewFakeChatModel(language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}))
	m := Observe(inner)

	bound, err := m.BindTools(nil)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, ok := bound.(*ObservedModel); !ok {
		t.Fatalf("bound model = %T", bound)
	}
}

// Restore paths re-set a previously installed model; wrapping again would
// break identity comparisons and double-record spans.
func TestObserveIdempotent(t *testing.T) {
	m := Observe(language.NewFakeChatModel())
	if Observe(m) != m {
		t.Fatal("Observe must return an already-observed model as-is")
	}
}

type failingLimiter struct{}

func (failingLimiter) Acquire(context.Context, bool) (bool, error) {
	return false, context.DeadlineExceeded
}

var _ ratelimiters.RateLimiter = failingLimiter{}
