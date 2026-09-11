package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// streamScript describes one Stream() call: chunks are delivered in order;
// when failAfter >= 0, err is returned after that many chunks (failAfter
// counting from 0: 0 means the first Next fails before any chunk).
type streamScript struct {
	chunks    []messages.Message
	failAfter int
	err       error
}

// flakyModel plays back scripts per Stream() call (cycling when exhausted)
// and counts calls. Invoke always errors — the retry/fallback paths under
// test are streaming.
type flakyModel struct {
	mu      sync.Mutex
	scripts []streamScript
	calls   int
}

func (m *flakyModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *flakyModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("invoke not used in stream tests")
}

func (m *flakyModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, errors.New("batch not used in stream tests")
}

func (m *flakyModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	script := m.scripts[m.calls%len(m.scripts)]
	m.calls++
	m.mu.Unlock()
	return &scriptStream{script: script}, nil
}

func (m *flakyModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *flakyModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}

func (m *flakyModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}

func (m *flakyModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

type scriptStream struct {
	script streamScript
	i      int
	failed bool
}

func (s *scriptStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.failed {
		return messages.Message{}, false, nil
	}
	if s.script.failAfter >= 0 && s.i >= s.script.failAfter {
		s.failed = true
		return messages.Message{}, false, s.script.err
	}
	if s.i >= len(s.script.chunks) {
		return messages.Message{}, false, nil
	}
	chunk := s.script.chunks[s.i]
	s.i++
	return chunk, true, nil
}

func (s *scriptStream) Close() error { return nil }

var errOverloaded = errors.New("529 overloaded_error")

func textChunk(s string) messages.Message {
	m := messages.AI("")
	m.Content = s
	return m
}

// drain consumes the stream and returns delivered chunks.
func drain(ctx context.Context, st runnables.Stream[messages.Message]) ([]messages.Message, error) {
	var out []messages.Message
	for {
		chunk, ok, err := st.Next(ctx)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, chunk)
	}
}

func shortBackoff(t *testing.T) {
	t.Helper()
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })
}

func TestStreamRetryBeforeAnyChunk(t *testing.T) {
	shortBackoff(t)
	primary := &flakyModel{scripts: []streamScript{
		{failAfter: 0, err: errOverloaded}, // fails before any chunk
		{failAfter: -1, chunks: []messages.Message{textChunk("a"), textChunk("b")}},
	}}
	r, err := New(Config{Primary: primary, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chunks, err := drain(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if primary.streamCalls() != 2 {
		t.Fatalf("primary stream calls = %d, want 2 (1 fail + 1 retry)", primary.streamCalls())
	}
	// A retry before any output was delivered is fully transparent: no
	// restart marker, only the successful attempt's chunks.
	for _, c := range chunks {
		if IsRestartChunk(c) {
			t.Fatalf("unexpected restart marker: %+v", c)
		}
	}
	if len(chunks) != 2 || TextOf(chunks[0]) != "a" || TextOf(chunks[1]) != "b" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TextOf(m messages.Message) string { return m.Content }

func TestStreamFallbackAfterRetries(t *testing.T) {
	shortBackoff(t)
	primary := &flakyModel{scripts: []streamScript{
		{failAfter: 1, chunks: []messages.Message{textChunk("partial")}, err: errOverloaded},
	}}
	fallback := &flakyModel{scripts: []streamScript{
		{failAfter: -1, chunks: []messages.Message{textChunk("fb1"), textChunk("fb2")}},
	}}
	var fallbackNotified int
	r, err := New(Config{
		Primary:    primary,
		Fallback:   fallback,
		MaxRetries: 1,
		OnFallback: func(error) { fallbackNotified++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chunks, err := drain(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}

	// Primary is tried maxRetries+1 times total before the switch.
	if got := primary.streamCalls(); got != 2 {
		t.Fatalf("primary stream calls = %d, want 2", got)
	}
	if got := fallback.streamCalls(); got != 1 {
		t.Fatalf("fallback stream calls = %d, want 1", got)
	}
	if fallbackNotified != 1 {
		t.Fatalf("onFallback called %d times, want 1", fallbackNotified)
	}

	// Sequence: the failed primary attempt's partial text, a retry marker,
	// the retried primary attempt's partial text (it fails again), the
	// fallback marker, then the fallback model's clean output.
	if len(chunks) != 6 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if TextOf(chunks[0]) != "partial" || TextOf(chunks[2]) != "partial" {
		t.Fatalf("primary partials = %q, %q", TextOf(chunks[0]), TextOf(chunks[2]))
	}
	if !IsRestartChunk(chunks[1]) || !strings.Contains(RestartReason(chunks[1]), "retrying") {
		t.Fatalf("chunk1 = %+v, want retry marker", chunks[1])
	}
	if !IsRestartChunk(chunks[3]) || !strings.Contains(RestartReason(chunks[3]), "fallback") {
		t.Fatalf("chunk3 = %+v, want fallback marker", chunks[3])
	}
	if TextOf(chunks[4]) != "fb1" || TextOf(chunks[5]) != "fb2" {
		t.Fatalf("fallback chunks = %+v", chunks[4:])
	}
}

func TestStreamNonRetryableError(t *testing.T) {
	shortBackoff(t)
	primary := &flakyModel{scripts: []streamScript{
		{failAfter: 0, err: errors.New("invalid api key")},
	}}
	r, err := New(Config{Primary: primary, MaxRetries: 3, Retryable: IsRetryable})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = drain(context.Background(), st)
	if err == nil {
		t.Fatal("expected the non-retryable error to surface")
	}
	if got := primary.streamCalls(); got != 1 {
		t.Fatalf("primary stream calls = %d, want 1 (no retry)", got)
	}
}

func TestStreamNoFallbackExhausted(t *testing.T) {
	shortBackoff(t)
	primary := &flakyModel{scripts: []streamScript{
		{failAfter: 0, err: errOverloaded},
	}}
	r, err := New(Config{Primary: primary, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = drain(context.Background(), st)
	if err == nil {
		t.Fatal("expected the exhausted error to surface")
	}
	if got := primary.streamCalls(); got != 2 {
		t.Fatalf("primary stream calls = %d, want 2 (attempt + retry)", got)
	}
}

func TestStreamCancelledContextNoRetry(t *testing.T) {
	shortBackoff(t)
	// A bare context.Canceled with a LIVE consumer ctx is a connection-layer
	// failure (e.g. a gateway closing a reused keep-alive connection under
	// the request), not a user cancellation — it must be retried. Only a
	// cancelled consumer ctx skips retries (TestStreamRealCancelIsNotRetried
	// in retrycancel_test.go).
	primary := &flakyModel{scripts: []streamScript{
		{failAfter: 0, err: context.Canceled},
	}}
	r, err := New(Config{Primary: primary, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = drain(context.Background(), st)
	if err == nil {
		t.Fatal("expected the cancellation error to surface after exhausting retries")
	}
	if got := primary.streamCalls(); got != 4 {
		t.Fatalf("primary stream calls = %d, want 4 (initial + 3 retries on connection-layer cancel)", got)
	}
}

func TestRestartChunkHelpers(t *testing.T) {
	marker := RestartChunk("switched to fallback model after error: boom")
	if !IsRestartChunk(marker) {
		t.Fatal("marker not detected")
	}
	if reason := RestartReason(marker); reason != "switched to fallback model after error: boom" {
		t.Fatalf("reason = %q", reason)
	}
	plain := textChunk("hello")
	if IsRestartChunk(plain) {
		t.Fatal("plain text chunk misdetected as restart marker")
	}
	// Round-tripping through JSON (as checkpoints do) keeps the marker.
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	var back messages.Message
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !IsRestartChunk(back) {
		t.Fatal("marker lost through JSON round trip")
	}
}
