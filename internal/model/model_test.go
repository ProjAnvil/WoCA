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

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("primary is required")
	}
	r, err := New(Config{Primary: language.NewFakeChatModel()})
	if err != nil {
		t.Fatal(err)
	}
	if r.maxRetries != 2 {
		t.Fatalf("default retries = %d", r.maxRetries)
	}
}

func TestIsRetryableKeywords(t *testing.T) {
	for _, e := range []string{"overloaded", "rate limited", "429 too many", "500", "502", "503", "504", "timeout", "connection refused", "temporarily unavailable"} {
		if !IsRetryable(errors.New(e)) {
			t.Errorf("IsRetryable(%q) = false", e)
		}
	}
	for _, e := range []string{"invalid request", "bad json", ""} {
		var err error
		if e == "" {
			err = nil
		} else {
			err = errors.New(e)
		}
		if IsRetryable(err) {
			t.Errorf("IsRetryable(%q) = true", e)
		}
	}
}

// failingModel always errors, delegating schema/capability queries.
type failingModel struct{ inner language.ChatModel }

func (m *failingModel) Invoke(ctx context.Context, msgs []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("overloaded")
}
func (m *failingModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}
func (m *failingModel) InputSchema() schema.Schema                           { return m.inner.InputSchema() }
func (m *failingModel) OutputSchema() schema.Schema                          { return m.inner.OutputSchema() }
func (m *failingModel) Capabilities() language.ChatModelCapabilities         { return m.inner.Capabilities() }
func (m *failingModel) BindTools(t []tools.Tool) (language.ChatModel, error) { return m, nil }
func (m *failingModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return m.inner.Batch(ctx, inputs, opts...)
}

func TestInvokeRetriesAndFallback(t *testing.T) {
	// Primary always fails; a non-retryable error stops retries early and
	// the fallback answers.
	calls := 0
	r, err := New(Config{
		Primary:    &failingModel{inner: language.NewFakeChatModel()},
		Fallback:   language.NewFakeChatModel(language.WithResponses(messages.AI("fb-ok"))),
		Retryable:  func(error) bool { return false },
		OnFallback: func(err error) { calls++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Invoke(context.Background(), []messages.Message{messages.Human("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "fb-ok" {
		t.Fatalf("out = %+v", out)
	}
	if calls != 1 {
		t.Fatalf("onFallback calls = %d", calls)
	}

	// Batch: mixed inputs still produce one result per input and the first
	// error when no fallback exists.
	r2, _ := New(Config{Primary: language.NewFakeChatModel(language.WithResponses(messages.AI("b1"), messages.AI("b2")))})
	outs, err := r2.Batch(context.Background(), [][]messages.Message{
		{messages.Human("a")},
		{messages.Human("b")},
	})
	if err != nil || len(outs) != 2 {
		t.Fatalf("batch = %+v %v", outs, err)
	}
}

func TestRouterDelegations(t *testing.T) {
	primary := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("ok")),
	)
	r, _ := New(Config{Primary: primary})
	if !r.Capabilities().ToolCalling {
		t.Fatal("capabilities not delegated")
	}
	_ = r.InputSchema()
	_ = r.OutputSchema()
	// BindTools returns a new bound router.
	bound, err := r.BindTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound == nil {
		t.Fatal("bound router nil")
	}
	if _, err := r.Invoke(context.Background(), []messages.Message{messages.Human("x")}); err != nil {
		t.Fatal(err)
	}
}

// nextDelay follows the capped exponential ladder.
func TestNextDelayLadder(t *testing.T) {
	base := retryBaseDelay
	for i, want := range []time.Duration{base, 2 * base, 4 * base, 8 * base, 8 * time.Second, 8 * time.Second} {
		if got := nextDelay(i + 1); got != want {
			t.Fatalf("nextDelay(%d) = %v want %v", i+1, got, want)
		}
	}
}

// BindTools delegates to the primary model.
func TestRouterBindTools(t *testing.T) {
	fake := language.NewFakeChatModel(language.WithResponses(messages.AI("x")))
	r, err := New(Config{Primary: fake})
	if err != nil {
		t.Fatal(err)
	}
	r.BindTools(nil) // must not panic
}
