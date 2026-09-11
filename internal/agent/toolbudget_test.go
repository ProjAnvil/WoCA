package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func contentLen(msgs []messages.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func TestToolResultBudgetUnderBudgetUnchanged(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("q"),
		messages.AI("thinking"),
		messages.Tool("1", "small result"),
		messages.Tool("2", strings.Repeat("x", 5000)),
	}
	out := ApplyToolResultBudget(msgs, DefaultToolResultBudgetChars)
	for i := range out {
		if out[i].Content != msgs[i].Content {
			t.Fatalf("message %d mutated under budget", i)
		}
	}
	if out[2].ToolCallID != "1" || out[3].ToolCallID != "2" {
		t.Fatal("tool call IDs not preserved")
	}
}

func TestToolResultBudgetReplacesLargestFirst(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("q"),
		messages.AI("call tools"),
		messages.Tool("small", strings.Repeat("s", 3000)), // kept: small
		messages.Tool("big", strings.Repeat("b", 80_000)), // replaced: largest
		messages.Tool("mid", strings.Repeat("m", 40_000)), // kept: fits after big removed
	}
	const budget = 50_000
	out := ApplyToolResultBudget(msgs, budget)

	if got := contentLen(out); got > budget+2*ToolResultPreviewChars {
		t.Fatalf("budget exceeded: %d chars", got)
	}
	if out[2].Content != msgs[2].Content {
		t.Fatal("small result should not be replaced")
	}
	if !strings.HasPrefix(out[3].Content, toolResultPlaceholderPrefix) {
		t.Fatal("largest result not replaced")
	}
	if out[3].ToolCallID != "big" {
		t.Fatal("replaced message lost its tool call ID")
	}
	if !strings.Contains(out[3].Content, strings.Repeat("b", 100)) {
		t.Fatal("replacement missing the preview content")
	}
	if !strings.Contains(out[3].Content, "80000 characters removed") {
		t.Fatal("replacement missing the removed-size note")
	}
	if out[4].Content != msgs[4].Content {
		t.Fatal("mid result should survive once the largest is replaced")
	}
	// The input slice is never mutated.
	if msgs[3].Content != strings.Repeat("b", 80_000) {
		t.Fatal("input slice mutated")
	}
}

func TestToolResultBudgetIdempotent(t *testing.T) {
	msgs := []messages.Message{
		messages.AI("calls"),
		messages.Tool("big", strings.Repeat("b", 300_000)),
		messages.Tool("big2", strings.Repeat("c", 200_000)),
	}
	once := ApplyToolResultBudget(msgs, DefaultToolResultBudgetChars)
	twice := ApplyToolResultBudget(once, DefaultToolResultBudgetChars)
	if len(once) != len(twice) {
		t.Fatalf("length changed on second pass: %d vs %d", len(once), len(twice))
	}
	for i := range once {
		if once[i].Content != twice[i].Content {
			t.Fatalf("second pass re-replaced message %d", i)
		}
	}
}

func TestToolResultBudgetRoundsIndependent(t *testing.T) {
	// Two separate tool rounds, each under budget: untouched even though
	// their combined size exceeds the budget (the budget scope is
	// per-message).
	msgs := []messages.Message{
		messages.AI("round 1"),
		messages.Tool("a", strings.Repeat("a", 40_000)),
		messages.AI("round 2"),
		messages.Tool("b", strings.Repeat("b", 40_000)),
	}
	out := ApplyToolResultBudget(msgs, 50_000)
	if out[1].Content != msgs[1].Content || out[3].Content != msgs[3].Content {
		t.Fatal("independent rounds should not be affected by other rounds")
	}
}

func TestToolResultBudgetFrozenRoundNotRetouched(t *testing.T) {
	// A round that fit when the model first saw it stays unreplaced on later
	// turns (deterministic freeze), even as later rounds overflow.
	round1 := []messages.Message{
		messages.AI("round 1"),
		messages.Tool("old", strings.Repeat("o", 40_000)),
	}
	first := ApplyToolResultBudget(round1, 50_000)
	if first[1].Content != round1[1].Content {
		t.Fatal("round under budget should pass through")
	}
	later := append(append([]messages.Message{}, first...),
		messages.AI("round 2"),
		messages.Tool("new", strings.Repeat("n", 80_000)),
	)
	out := ApplyToolResultBudget(later, 50_000)
	if out[1].Content != round1[1].Content {
		t.Fatal("previously seen round was retroactively replaced")
	}
	if !strings.HasPrefix(out[3].Content, toolResultPlaceholderPrefix) {
		t.Fatal("overflowing new round not replaced")
	}
}

func TestToolResultBudgetDisabled(t *testing.T) {
	msgs := []messages.Message{messages.Tool("big", strings.Repeat("b", 500_000))}
	out := ApplyToolResultBudget(msgs, -1)
	if out[0].Content != msgs[0].Content {
		t.Fatal("negative budget must disable enforcement")
	}
}

func TestTruncateRunesBoundaries(t *testing.T) {
	s := "hello, 世界"
	got := truncateRunes(s, 8)
	if got != "hello, 世" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if truncateRunes(s, 100) != s {
		t.Fatal("short string should pass through")
	}
}

// captureModel records the message list of every model request.
type captureModel struct {
	mu       sync.Mutex
	requests [][]messages.Message
}

func (m *captureModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.AI("ok"), nil
}

func (m *captureModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}

func (m *captureModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	m.requests = append(m.requests, append([]messages.Message(nil), input...))
	m.mu.Unlock()
	return &singleChunkStream{chunk: messages.AI("done")}, nil
}

func (m *captureModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *captureModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *captureModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *captureModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

type singleChunkStream struct {
	chunk messages.Message
	done  bool
}

func (s *singleChunkStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.done {
		return messages.Message{}, false, nil
	}
	s.done = true
	return s.chunk, true, nil
}

func (s *singleChunkStream) Close() error { return nil }

// TestModelNodeAppliesToolResultBudget verifies the model request is
// assembled with the budget applied while the transcript keeps originals.
func TestModelNodeAppliesToolResultBudget(t *testing.T) {
	model := &captureModel{}
	a, err := New(Config{
		Model:                 model,
		Registry:              tools.NewRegistry(t.TempDir()),
		SystemPrompt:          "test",
		PermissionMode:        config.ModeBypass,
		ToolResultBudgetChars: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	history := []messages.Message{
		messages.Human("run"),
		messages.AI("using tools"),
		messages.Tool("t1", strings.Repeat("r", 30_000)),
	}
	out, err := a.Invoke(context.Background(), history, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	// The model saw the budgeted version.
	saw := model.requests[0]
	var replaced bool
	for _, m := range saw {
		if m.ToolCallID == "t1" {
			if strings.HasPrefix(m.Content, toolResultPlaceholderPrefix) {
				replaced = true
			}
		}
	}
	if !replaced {
		t.Fatal("model request not budgeted")
	}
	// The transcript keeps the original content.
	var original bool
	for _, m := range out {
		if m.ToolCallID == "t1" && len(m.Content) == 30_000 {
			original = true
		}
	}
	if !original {
		t.Fatal("transcript lost the original tool result")
	}
}
