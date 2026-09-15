package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newQueueTestAgent builds an agent whose model requests one Sleep call then
// finishes, using the shared recorder-based Sleep tool.
func newQueueTestAgent(t *testing.T, rec *recorder, sleepMS int) *Agent {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Sleep", rec))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "q1", Name: "Sleep", Args: map[string]any{"ms": sleepMS}},
			}},
			messages.AI("all done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestQueuedInputInjectedAfterToolResults(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 10)
	a.QueueInput("actually, also check the tests")

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
	if err != nil {
		t.Fatal(err)
	}

	var toolIdx, queuedIdx, finalIdx = -1, -1, -1
	for i, m := range out {
		switch {
		case m.Role == messages.RoleTool && m.ToolCallID == "q1":
			toolIdx = i
		case m.Role == messages.RoleHuman && strings.Contains(m.Content, "<queued-user-input>") &&
			strings.Contains(m.Content, "actually, also check the tests"):
			queuedIdx = i
		case m.Role == messages.RoleAI && m.Content == "all done":
			finalIdx = i
		}
	}
	if toolIdx == -1 || queuedIdx == -1 || finalIdx == -1 {
		t.Fatalf("transcript missing pieces: tool=%d queued=%d final=%d (%+v)", toolIdx, queuedIdx, finalIdx, out)
	}
	// The queued input is attached strictly after the tool results (API
	// ordering) and before the model's next response.
	if queuedIdx < toolIdx || finalIdx < queuedIdx {
		t.Fatalf("ordering wrong: tool=%d queued=%d final=%d", toolIdx, queuedIdx, finalIdx)
	}
	if a.QueuedInputCount() != 0 {
		t.Fatal("queue not drained")
	}
}

func TestQueuedInputDuringToolExecution(t *testing.T) {
	rec := &recorder{}
	a := newQueueTestAgent(t, rec, 300)

	type result struct {
		out []messages.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
		done <- result{out, err}
	}()

	// Submit while the tool is still sleeping.
	time.Sleep(80 * time.Millisecond)
	a.QueueInput("mid-turn correction")

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	var sawQueued bool
	for _, m := range res.out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "mid-turn correction") {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatal("input submitted during tool execution was not attached")
	}
	if rec.count() != 1 {
		t.Fatalf("Sleep ran %d times, want 1", rec.count())
	}
}

func TestNoQueuedInputNoExtraMessages(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 10)
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "<queued-user-input>") {
			t.Fatal("unexpected queued-input message with an empty queue")
		}
	}
	// system, human, AI(toolcall), tool, AI = 5.
	if len(out) != 5 {
		t.Fatalf("transcript length = %d: %+v", len(out), out)
	}
}

func TestQueueDrainSemantics(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 1)
	a.QueueInput("first")
	a.QueueInput("  ") // whitespace-only: ignored
	a.QueueInput("second")
	if got := a.QueuedInputCount(); got != 2 {
		t.Fatalf("queued count = %d, want 2", got)
	}
	items := a.DrainQueuedInputs()
	if len(items) != 2 || items[0].Text != "first" || items[1].Text != "second" {
		t.Fatalf("drain = %+v", items)
	}
	if got := a.QueuedInputCount(); got != 0 {
		t.Fatalf("queue not cleared: %d", got)
	}
	if again := a.DrainQueuedInputs(); len(again) != 0 {
		t.Fatalf("second drain = %+v", again)
	}
}

func TestQueueInputConcurrent(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 1)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			a.QueueInput(strings.Repeat("x", i+1))
		})
	}
	wg.Wait()
	if got := a.QueuedInputCount(); got != 32 {
		t.Fatalf("queued count = %d, want 32", got)
	}
}
