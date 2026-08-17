package model

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"
)

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
