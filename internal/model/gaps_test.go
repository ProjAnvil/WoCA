package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"
)

// startFailModel fails Stream() itself (as opposed to failing on Next) for
// the first len(errs) calls, then streams chunks from a script.
type startFailModel struct {
	mu    sync.Mutex
	errs  []error
	calls int
	chunk string
}

func (m *startFailModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *startFailModel) Invoke(context.Context, []messages.Message, ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("invoke not used")
}

func (m *startFailModel) Batch(context.Context, [][]messages.Message, ...runnables.Option) ([]messages.Message, error) {
	return nil, errors.New("batch not used")
}

func (m *startFailModel) Stream(_ context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	if i < len(m.errs) {
		return nil, m.errs[i]
	}
	return &scriptStream{script: streamScript{failAfter: -1, chunks: []messages.Message{textChunk(m.chunk)}}}, nil
}

func (m *startFailModel) InputSchema() schema.Schema  { return schema.Schema{} }
func (m *startFailModel) OutputSchema() schema.Schema { return schema.Schema{} }
func (m *startFailModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{}
}
func (m *startFailModel) BindTools([]tools.Tool) (language.ChatModel, error) { return m, nil }

// bindErrModel rejects BindTools.
type bindErrModel struct{ inner language.ChatModel }

func (m *bindErrModel) Invoke(ctx context.Context, in []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return m.inner.Invoke(ctx, in, opts...)
}
func (m *bindErrModel) Batch(ctx context.Context, in [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return m.inner.Batch(ctx, in, opts...)
}
func (m *bindErrModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return m.inner.Stream(ctx, in, opts...)
}
func (m *bindErrModel) InputSchema() schema.Schema                              { return m.inner.InputSchema() }
func (m *bindErrModel) OutputSchema() schema.Schema                             { return m.inner.OutputSchema() }
func (m *bindErrModel) Capabilities() language.ChatModelCapabilities            { return m.inner.Capabilities() }
func (m *bindErrModel) BindTools([]tools.Tool) (language.ChatModel, error) {
	return nil, errors.New("bind: tools not supported")
}

// Without a fallback, Invoke surfaces the primary error after retries are
// skipped/exhausted, and Batch records the first error while still returning
// one (zero) result per input.
func TestInvokeNoFallbackSurfacesErrorAndBatchRecordsFirst(t *testing.T) {
	r, err := New(Config{
		Primary:   &failingModel{inner: language.NewFakeChatModel()},
		Retryable: func(error) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Invoke(context.Background(), []messages.Message{messages.Human("hi")}); err == nil {
		t.Fatal("expected error without fallback")
	}

	outs, err := r.Batch(context.Background(), [][]messages.Message{
		{messages.Human("a")},
		{messages.Human("b")},
	})
	if err == nil {
		t.Fatal("Batch must surface the first invoke error")
	}
	if len(outs) != 2 {
		t.Fatalf("Batch returned %d results, want one per input", len(outs))
	}
}

func TestRestartReasonWithoutMarkerOrNonStringReason(t *testing.T) {
	if got := RestartReason(textChunk("plain")); got != "" {
		t.Fatalf("RestartReason(plain) = %q, want empty", got)
	}
	// A restart block whose reason is not a string yields no reason.
	m := messages.AI("")
	m.ContentBlocks = []messages.ContentBlock{messages.NonStandardContentBlock{
		Type:  RestartBlockType,
		Value: map[string]any{"reason": 42},
	}}
	if got := RestartReason(m); got != "" {
		t.Fatalf("RestartReason(non-string) = %q, want empty", got)
	}
}

// Stream() failing at open (not mid-stream) is retried like a mid-stream
// failure when the error is transient.
func TestStreamOpenErrorRetried(t *testing.T) {
	shortBackoff(t)
	primary := &startFailModel{errs: []error{errOverloaded}, chunk: "recovered"}
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
		t.Fatalf("transient open error must be retried: %v", err)
	}
	if len(chunks) != 1 || TextOf(chunks[0]) != "recovered" {
		t.Fatalf("chunks = %+v", chunks)
	}
	if primary.streamCalls() != 2 {
		t.Fatalf("Stream calls = %d, want 2", primary.streamCalls())
	}
}

// A non-retryable Stream() open error surfaces immediately.
func TestStreamOpenErrorNonRetryable(t *testing.T) {
	shortBackoff(t)
	primary := &startFailModel{errs: []error{errors.New("invalid api key")}}
	r, err := New(Config{Primary: primary, MaxRetries: 2, Retryable: IsRetryable})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.Next(context.Background()); err == nil {
		t.Fatal("expected the open error to surface")
	}
	if got := primary.streamCalls(); got != 1 {
		t.Fatalf("Stream calls = %d, want 1 (no retry)", got)
	}
}

// Next after exhaustion keeps reporting a clean EOF.
func TestStreamNextAfterDone(t *testing.T) {
	primary := &startFailModel{chunk: "only"}
	r, err := New(Config{Primary: primary})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chunks, err := drain(context.Background(), st)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks = %+v err = %v", chunks, err)
	}
	chunk, ok, err := st.Next(context.Background())
	if ok || err != nil || chunk.Content != "" {
		t.Fatalf("Next after done = (%q, %v, %v), want zero, false, nil", chunk.Content, ok, err)
	}
}

// Closing a stream that never started is a no-op.
func TestStreamCloseBeforeStart(t *testing.T) {
	r, err := New(Config{Primary: &startFailModel{chunk: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close before start = %v", err)
	}
}

// A consumer cancellation during the backoff window aborts the retry.
func TestStreamBackoffInterruptedByCancel(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = 200 * time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })

	primary := &flakyModel{scripts: []streamScript{{failAfter: 0, err: errOverloaded}}}
	r, err := New(Config{Primary: primary, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := r.Stream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, _, err := st.Next(context.Background()); err == nil {
		t.Fatal("expected cancellation to surface the error")
	}
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("retry waited the full backoff (%v); cancel did not interrupt", elapsed)
	}
	if got := primary.streamCalls(); got != 1 {
		t.Fatalf("primary stream calls = %d, want 1", got)
	}
}

func TestBindToolsErrorPaths(t *testing.T) {
	fake := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))

	// Primary bind failure.
	r, err := New(Config{Primary: &bindErrModel{inner: fake}, Fallback: fake})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.BindTools(nil); err == nil {
		t.Fatal("expected primary BindTools error to surface")
	}

	// Fallback bind failure.
	r2, err := New(Config{Primary: fake, Fallback: &bindErrModel{inner: fake}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.BindTools(nil); err == nil {
		t.Fatal("expected fallback BindTools error to surface")
	}

	// Success with a fallback: the bound router keeps a bound fallback.
	r3, err := New(Config{Primary: fake, Fallback: language.NewFakeChatModel()})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := r3.BindTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	br, ok := bound.(*Router)
	if !ok {
		t.Fatalf("bound = %T, want *Router", bound)
	}
	if br.fallback == nil {
		t.Fatal("bound router lost its fallback")
	}
}
