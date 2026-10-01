package sdk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
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

// allPowerPlugin implements every plugin capability at once: lifecycle
// observation, tool gating, and model-round transformation.
type allPowerPlugin struct {
	events   []RunEvent
	guardSaw []ToolCallInfo
	turns    []*ModelTurn
}

func (p *allPowerPlugin) Name() string { return "all-power" }

func (p *allPowerPlugin) Observe(_ context.Context, ev RunEvent) { p.events = append(p.events, ev) }

func (p *allPowerPlugin) BeforeTool(_ context.Context, call ToolCallInfo) ToolDecision {
	p.guardSaw = append(p.guardSaw, call)
	return DenyTool("denied by sdk guard")
}

func (p *allPowerPlugin) AfterTool(_ context.Context, call ToolCallInfo, _ ToolResultInfo) ToolPostDecision {
	return ToolPostDecision{}
}

func (p *allPowerPlugin) BeforeModel(_ context.Context, turn *ModelTurn) BeforeModelResult {
	p.turns = append(p.turns, turn)
	return BeforeModelResult{PrependContext: "quarterly revenue is up"}
}

func (p *allPowerPlugin) AfterModel(_ context.Context, _ *ModelTurn, reply *ModelReply) *ModelReply {
	if reply.Text == "" {
		return nil // tool-call rounds keep their empty text
	}
	return &ModelReply{Text: "sanitized reply"}
}

// toolThenTextFake answers the first /v1/messages with a Bash tool_use and
// every later one with plain text, recording each request body.
func toolThenTextFake(t *testing.T) (srvURL string, bodies func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		n++
		idx := n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 1 {
			fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo hi"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`)
			return
		}
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"all done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestQueryPluginFullChain: one plugin with all three capabilities wired
// through sdk.Query — the guard's deny feeds back to the model, the model
// hook's context reaches the request, and its replacement reaches the
// result.
func TestQueryPluginFullChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	url, bodies := toolThenTextFake(t)
	p := &allPowerPlugin{}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	s, err := Query(ctx, "run the echo",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(url),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
		WithPermissionMode("bypassPermissions"),
		WithPlugins(p),
	)
	if err != nil {
		t.Fatal(err)
	}
	var res *Result
	for ev := range s.Events() {
		if ev.Type == EventResult {
			res = ev.Result
		}
	}
	if res == nil {
		t.Fatal("no result event")
	}
	if res.IsError {
		t.Fatalf("run errored: %s", res.ErrorText)
	}
	if res.Text != "sanitized reply" {
		t.Fatalf("result text = %q, want the AfterModel replacement", res.Text)
	}
	if len(p.guardSaw) != 1 || p.guardSaw[0].Name != "Bash" {
		t.Fatalf("guard saw %+v", p.guardSaw)
	}
	seen := bodies()
	if len(seen) < 2 {
		t.Fatalf("model requests = %d, want >= 2", len(seen))
	}
	if !strings.Contains(seen[1], "denied by sdk guard") {
		t.Fatalf("second request missing the guard's deny feedback: %s", seen[1])
	}
	var hasContext bool
	for _, b := range seen {
		if strings.Contains(b, "quarterly revenue is up") {
			hasContext = true
		}
	}
	if !hasContext {
		t.Fatalf("no request carried the injected context: %v", seen)
	}
	if len(p.turns) == 0 || p.turns[0].Prompt != "run the echo" {
		t.Fatalf("model turns = %+v", p.turns)
	}
	var start, end bool
	for _, ev := range p.events {
		switch ev.Kind {
		case RunEventStart:
			start = true
		case RunEventEnd:
			end = true
		}
	}
	if !start || !end {
		t.Fatalf("plugin lifecycle events incomplete: %+v", p.events)
	}
}

// wrapPlugin wraps the model with a pass-through counter.
type wrapPlugin struct{ streams, factories int }

type passthroughModel struct {
	language.ChatModel
	p *wrapPlugin
}

func (m *passthroughModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.p.streams++
	return m.ChatModel.Stream(ctx, in, opts...)
}

func (p *wrapPlugin) Name() string { return "wrap" }

func (p *wrapPlugin) WrapModel(m ChatModel) ChatModel {
	p.factories++
	return &passthroughModel{ChatModel: m, p: p}
}

// TestQueryModelWrapper: a WrapModel plugin wraps the model through the
// full sdk assembly (and survives the tool bind).
func TestQueryModelWrapper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv := fakeAnthropic(t)
	defer srv.Close()

	p := &wrapPlugin{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
		WithPlugins(p),
	)
	if err != nil {
		t.Fatal(err)
	}
	var res *Result
	for ev := range s.Events() {
		if ev.Type == EventResult {
			res = ev.Result
		}
	}
	if res == nil || res.IsError || res.Text != "hello from fake" {
		t.Fatalf("result = %+v", res)
	}
	if p.streams != 1 || p.factories == 0 {
		t.Fatalf("wrapper streams=%d factories=%d, want the model call through the wrapper", p.streams, p.factories)
	}
}
