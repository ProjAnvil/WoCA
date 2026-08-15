package model

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"
)

// countingModel fails failuresLeft times with a transient error, then
// delegates to inner.
type countingModel struct {
	mu           sync.Mutex
	failuresLeft int
	calls        int
	inner        language.ChatModel
}

func (m *countingModel) Invoke(ctx context.Context, msgs []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	m.mu.Lock()
	m.calls++
	if m.failuresLeft > 0 {
		m.failuresLeft--
		m.mu.Unlock()
		return messages.Message{}, errors.New("overloaded: try again")
	}
	m.mu.Unlock()
	return m.inner.Invoke(ctx, msgs, opts...)
}

func (m *countingModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *countingModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}

func (m *countingModel) InputSchema() schema.Schema  { return m.inner.InputSchema() }
func (m *countingModel) OutputSchema() schema.Schema { return m.inner.OutputSchema() }
func (m *countingModel) Capabilities() language.ChatModelCapabilities {
	return m.inner.Capabilities()
}
func (m *countingModel) BindTools(t []tools.Tool) (language.ChatModel, error) { return m, nil }

func TestRouterRetriesThenSucceeds(t *testing.T) {
	inner := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	primary := &countingModel{failuresLeft: 2, inner: inner}

	r, err := New(Config{Primary: primary, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Invoke(context.Background(), []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Fatalf("resp = %q", resp.Content)
	}
	if primary.calls != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", primary.calls)
	}
}

func TestRouterFallsBack(t *testing.T) {
	primary := &countingModel{failuresLeft: 99, inner: language.NewFakeChatModel()}
	fallback := language.NewFakeChatModel(language.WithResponses(messages.AI("from fallback")))

	fallbacks := 0
	r, err := New(Config{
		Primary:    primary,
		Fallback:   fallback,
		MaxRetries: 1,
		OnFallback: func(err error) { fallbacks++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Invoke(context.Background(), []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "from fallback" {
		t.Fatalf("resp = %q", resp.Content)
	}
	if fallbacks != 1 {
		t.Fatalf("OnFallback called %d times, want 1", fallbacks)
	}
}

func TestIsRetryable(t *testing.T) {
	if !IsRetryable(errors.New("rate limit exceeded")) {
		t.Error("rate limit should be retryable")
	}
	if !IsRetryable(errors.New("HTTP 503")) {
		t.Error("503 should be retryable")
	}
	if IsRetryable(errors.New("invalid request")) {
		t.Error("invalid request should not be retryable")
	}
	if IsRetryable(nil) {
		t.Error("nil should not be retryable")
	}
}
