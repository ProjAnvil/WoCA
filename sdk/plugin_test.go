package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingPlugin captures every RunEvent for inspection.
type recordingPlugin struct {
	mu     sync.Mutex
	events []RunEvent
}

func (p *recordingPlugin) Name() string { return "recording" }

func (p *recordingPlugin) Observe(_ context.Context, ev RunEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPlugin) snapshot() []RunEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]RunEvent(nil), p.events...)
}

// nameOnlyPlugin implements Plugin without EventPlugin: Query must accept
// and ignore it.
type nameOnlyPlugin struct{}

func (nameOnlyPlugin) Name() string { return "name-only" }

func TestQueryDispatchesRunEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv := fakeAnthropic(t)
	defer srv.Close()

	rec := &recordingPlugin{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
		WithPlugins(rec, nameOnlyPlugin{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}

	evs := rec.snapshot()
	if len(evs) < 3 {
		t.Fatalf("expected run_start, message(s), run_end; got %d events: %+v", len(evs), evs)
	}
	start := evs[0]
	if start.Kind != RunEventStart {
		t.Fatalf("first event kind = %q, want %q", start.Kind, RunEventStart)
	}
	if start.Prompt != "say hi" {
		t.Fatalf("run_start prompt = %q, want %q", start.Prompt, "say hi")
	}
	if start.SessionID == "" || start.RunID == "" {
		t.Fatal("run_start event missing session/run identity")
	}
	if start.At.IsZero() {
		t.Fatal("run_start event missing timestamp")
	}
	for _, ev := range evs {
		if ev.SessionID != start.SessionID || ev.RunID != start.RunID {
			t.Fatalf("event %q has mismatched identity (session=%q run=%q)", ev.Kind, ev.SessionID, ev.RunID)
		}
	}
	last := evs[len(evs)-1]
	if last.Kind != RunEventEnd {
		t.Fatalf("last event kind = %q, want %q", last.Kind, RunEventEnd)
	}
	if last.Result == nil || last.Result.Subtype != "success" {
		t.Fatalf("run_end result = %+v", last.Result)
	}
	var sawAssistant bool
	for _, ev := range evs[1 : len(evs)-1] {
		if ev.Kind != RunEventMessage {
			t.Fatalf("middle event kind = %q, want %q", ev.Kind, RunEventMessage)
		}
		if ev.Message != nil && ev.Message.Role == "assistant" {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Fatal("no assistant message event observed")
	}
}
