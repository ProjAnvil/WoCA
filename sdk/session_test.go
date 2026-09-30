package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedFakeAnthropic answers every /v1/messages with the standard one-line
// completion, records each request body, and can hold responses back until
// released (to keep a turn observably in flight).
func gatedFakeAnthropic(t *testing.T) (srv *httptest.Server, release func(), bodies func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	gate := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		read := 0
		for read < len(buf) {
			n, err := r.Body.Read(buf[read:])
			read += n
			if err != nil {
				break
			}
		}
		mu.Lock()
		seen = append(seen, string(buf))
		mu.Unlock()
		<-gate
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`event: message_start
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

`))
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	return srv, func() { once.Do(func() { close(gate) }) }, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func sessionOpts(t *testing.T, srv *httptest.Server) []Option {
	return []Option{
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
	}
}

func drainEvents(t *testing.T, evs <-chan Event) *Result {
	t.Helper()
	var res *Result
	for ev := range evs {
		if ev.Type == EventResult {
			res = ev.Result
		}
	}
	if res == nil {
		t.Fatal("no result event")
	}
	return res
}

func TestSessionMultiTurnCarriesHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, release, bodies := gatedFakeAnthropic(t)
	release()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := NewSession(ctx, sessionOpts(t, srv)...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r1 := drainEvents(t, mustAsk(t, s, "first question"))
	if r1.IsError || r1.Text != "hello from fake" {
		t.Fatalf("turn 1 result = %+v", r1)
	}
	r2 := drainEvents(t, mustAsk(t, s, "second question"))
	if r2.IsError {
		t.Fatalf("turn 2 errored: %s", r2.ErrorText)
	}

	seen := bodies()
	if len(seen) != 2 {
		t.Fatalf("model requests = %d, want 2", len(seen))
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(seen[1]), &req); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	var hasHistory bool
	for _, m := range req.Messages {
		raw, _ := json.Marshal(m.Content)
		if strings.Contains(string(raw), "hello from fake") {
			hasHistory = true
		}
	}
	if !hasHistory {
		t.Fatalf("second request does not carry the first turn's reply: %s", seen[1])
	}
	if !strings.Contains(seen[1], "second question") {
		t.Fatalf("second request missing the new prompt: %s", seen[1])
	}
}

// TestSessionPerAskUsage: the result envelope reports THIS turn's usage, not
// the session cumulative totals.
func TestSessionPerAskUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, release, _ := gatedFakeAnthropic(t)
	release()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := NewSession(ctx, sessionOpts(t, srv)...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r1 := drainEvents(t, mustAsk(t, s, "one"))
	r2 := drainEvents(t, mustAsk(t, s, "two"))
	if r1.Usage.InputTokens != 10 || r2.Usage.InputTokens != 10 {
		t.Fatalf("per-ask usage = %+v / %+v, want input 10 on both", r1.Usage, r2.Usage)
	}
	if r2.Usage.Requests != 1 {
		t.Fatalf("turn 2 requests = %d, want 1 (cumulative would be 2)", r2.Usage.Requests)
	}
}

func TestSessionAskWhileBusy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, release, _ := gatedFakeAnthropic(t) // gated: turn 1 stays in flight

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := NewSession(ctx, sessionOpts(t, srv)...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	evs, err := s.Ask(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ask(ctx, "second"); err != ErrAskInProgress {
		t.Fatalf("second Ask error = %v, want ErrAskInProgress", err)
	}
	release()
	drainEvents(t, evs)
}

func TestSessionAskAfterClose(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, release, _ := gatedFakeAnthropic(t)
	release()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := NewSession(ctx, sessionOpts(t, srv)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ask(ctx, "late"); err != ErrSessionClosed {
		t.Fatalf("Ask after Close error = %v, want ErrSessionClosed", err)
	}
}

func mustAsk(t *testing.T, s *Session, prompt string) <-chan Event {
	t.Helper()
	evs, err := s.Ask(t.Context(), prompt)
	if err != nil {
		t.Fatalf("Ask(%q): %v", prompt, err)
	}
	return evs
}
