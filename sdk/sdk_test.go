package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeAnthropic answers every /v1/messages call with a one-line completion.
// Mirror the streaming SSE form the provider client expects; if the client
// requests non-streaming, the plain JSON body is equivalent. The reference
// pattern is captureAnthropic in internal/config/thinking_test.go.
func fakeAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from fake"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
}

func TestQueryStreamsAndFinishes(t *testing.T) {
	// Isolate HOME so BuildAgent never reads the developer's real
	// ~/.claude skills/settings into the run.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv := fakeAnthropic(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var sawAssistant bool
	var res *Result
	for ev := range s.Events() {
		switch ev.Type {
		case EventAssistant:
			sawAssistant = true
		case EventResult:
			res = ev.Result
		}
	}
	if res == nil {
		t.Fatal("no result event")
	}
	if res.IsError {
		t.Fatalf("run errored: %s", res.ErrorText)
	}
	if res.Text != "hello from fake" {
		t.Fatalf("result text = %q", res.Text)
	}
	if !sawAssistant {
		t.Fatal("no assistant event streamed")
	}
	// langchain-golang v0.7.0 yields a terminal usage-only chunk on
	// message_delta, which internal/agent folds into the run totals.
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

// TestCloseTwice verifies Close is idempotent: a second call must neither
// panic nor hang after the run has finished and been closed once.
func TestCloseTwice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv := fakeAnthropic(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
