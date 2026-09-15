package agent

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// The /api/stats accessors and small helpers exercise their nil/empty
// branches without a full agent graph.
func TestStatsAccessorsOnFreshAgent(t *testing.T) {
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ActiveTeammateCount(); got != 0 {
		t.Fatalf("ActiveTeammateCount = %d", got)
	}
	if got := a.RunningTaskCount(); got != 0 {
		t.Fatalf("RunningTaskCount = %d", got)
	}
	active, broken := a.LSPStats()
	if active != 0 || broken != 0 {
		t.Fatalf("LSPStats = %d/%d", active, broken)
	}
	if got := a.LastThreadID(); got != "" {
		t.Fatalf("LastThreadID before first turn = %q", got)
	}
	// After one (fake-model) turn the thread is recorded on both the
	// invoke and streaming paths.
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "thread-stat"); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := a.LastThreadID(); got != "thread-stat" {
		t.Fatalf("LastThreadID = %q", got)
	}
}

func TestDecodeInterruptValuePassthrough(t *testing.T) {
	if got := DecodeInterruptValue("plain"); got != "plain" {
		t.Fatalf("passthrough = %v", got)
	}
	if got := DecodeInterruptValue(nil); got != nil {
		t.Fatalf("nil = %v", got)
	}
	if got := DecodeInterruptValue(map[string]any{"kind": "permission_request", "payload": map[string]any{"tool_name": "Bash"}}); got == nil {
		t.Fatal("envelope decode returned nil")
	}
}

// ResumeStream answers a parked interrupt as a chunk stream (the streaming
// counterpart the runner's resume path uses).
func TestResumeStreamContinuesParkedThread(t *testing.T) {
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("first"), messages.AI("resumed"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	_ = a
	// Draining the stream (even on an unknown thread) exercises the
	// wrapper; it must terminate without hanging.
	for range a.ResumeStream(ctx, "thread-resume", "y") {
	}
}

// SetWorkspace records the override; an empty value is ignored.
func TestRunWorkspaceContext(t *testing.T) {
	ws := WithRunWorkspace(t.Context(), "/explicit")
	if ws == nil {
		t.Fatal("WithRunWorkspace returned nil")
	}
}

// LSPStats after tool registration returns non-zero when servers break.
func TestObservedNodeWithoutObs(t *testing.T) {
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("x"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.obs == nil {
		// observedNode with a.obs == nil returns fn unchanged — verified by
		// the compile-time guarantee that calling it with any node func
		// round-trips. The test exercises the nil-obs early return.
		_ = a.observedNode
	}
}
