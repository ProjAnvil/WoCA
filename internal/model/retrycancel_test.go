package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"
)

// cancelingStream mimics a gateway resetting a reused connection: the first
// stream fails with a bare context.Canceled while the consumer ctx is alive
// (exactly what DeepSeek-style endpoints do to the request that reuses a
// keep-alive connection closed right after a previous stream ended).
type cancelingStream struct {
	fails  int
	called int
}

func (s *cancelingStream) Next(context.Context) (messages.Message, bool, error) {
	s.called++
	if s.called <= s.fails {
		return messages.Message{}, false, context.Canceled
	}
	if s.called == s.fails+1 {
		return messages.Message{Role: messages.RoleAI, Content: "recovered"}, true, nil
	}
	return messages.Message{}, false, nil
}

func (s *cancelingStream) Close() error { return nil }

type cancelOnceModel struct {
	stream *cancelingStream
}

func (m *cancelOnceModel) Invoke(context.Context, []messages.Message, ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("unused")
}
func (m *cancelOnceModel) Batch(context.Context, [][]messages.Message, ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}
func (m *cancelOnceModel) Stream(_ context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return m.stream, nil
}
func (m *cancelOnceModel) InputSchema() schema.Schema  { return schema.Schema{} }
func (m *cancelOnceModel) OutputSchema() schema.Schema { return schema.Schema{} }
func (m *cancelOnceModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{}
}
func (m *cancelOnceModel) BindTools(t []tools.Tool) (language.ChatModel, error) { return m, nil }

func TestStreamRetriesConnectionLayerCancel(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })

	cs := &cancelingStream{fails: 1}
	r, err := New(Config{Primary: &cancelOnceModel{stream: cs}, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := r.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for {
		chunk, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("connection-layer cancel must be retried, got: %v", err)
		}
		if !ok {
			break
		}
		last = chunk.Content
	}
	if last != "recovered" || cs.called < 2 {
		t.Fatalf("recovered=%q called=%d, want recovered with a retry", last, cs.called)
	}
}

func TestStreamRealCancelIsNotRetried(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })

	cs := &cancelingStream{fails: 100}
	r, err := New(Config{Primary: &cancelOnceModel{stream: cs}, MaxRetries: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := r.Stream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, _, err = stream.Next(context.Background())
	if err == nil {
		t.Fatal("cancelled consumer ctx must surface the error")
	}
	if cs.called > 2 {
		t.Fatalf("cancelled consumer must not be retried, called=%d", cs.called)
	}
}
