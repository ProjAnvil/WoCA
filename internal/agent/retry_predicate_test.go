package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	wotools "github.com/projanvil/woca/internal/tools"
)

// alwaysFailModel fails every Stream call with the scripted error.
type alwaysFailModel struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (m *alwaysFailModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *alwaysFailModel) Invoke(context.Context, []messages.Message, ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, m.err
}

func (m *alwaysFailModel) Batch(context.Context, [][]messages.Message, ...runnables.Option) ([]messages.Message, error) {
	return nil, m.err
}

func (m *alwaysFailModel) Stream(context.Context, []messages.Message, ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return nil, m.err
}

func (m *alwaysFailModel) InputSchema() schema.Schema  { return schema.Schema{} }
func (m *alwaysFailModel) OutputSchema() schema.Schema { return schema.Schema{} }
func (m *alwaysFailModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{}
}
func (m *alwaysFailModel) BindTools([]tools.Tool) (language.ChatModel, error) { return m, nil }

// TestRouterSkipsNonRetryableErrors locks the assembly wiring: a permanent
// error (400-class) must not burn the retry budget — only transient
// overload/rate/5xx errors retry.
func TestRouterSkipsNonRetryableErrors(t *testing.T) {
	primary := &alwaysFailModel{err: errors.New("400 bad request: invalid model")}
	a, err := New(Config{
		Model:           primary,
		Registry:        wotools.NewRegistry(t.TempDir()),
		SystemPrompt:    "t",
		PermissionMode:  config.ModeBypass,
		ModelMaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("hi")}, "t")
	if err == nil || !strings.Contains(err.Error(), "400 bad request") {
		t.Fatalf("err = %v", err)
	}
	if got := primary.streamCalls(); got != 1 {
		t.Fatalf("stream calls = %d, want 1 (permanent errors must not retry)", got)
	}
}

// TestRouterRetriesTransientErrors is the positive counterpart: transient
// overload errors still exhaust the retry budget (1 + maxRetries calls).
func TestRouterRetriesTransientErrors(t *testing.T) {
	primary := &alwaysFailModel{err: errors.New("529 overloaded")}
	a, err := New(Config{
		Model:           primary,
		Registry:        wotools.NewRegistry(t.TempDir()),
		SystemPrompt:    "t",
		PermissionMode:  config.ModeBypass,
		ModelMaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("hi")}, "t")
	if err == nil {
		t.Fatal("expected the transient error to surface after retries")
	}
	if got := primary.streamCalls(); got != 3 {
		t.Fatalf("stream calls = %d, want 3 (1 + 2 retries)", got)
	}
}
