package runner

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

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// budgetModel plays scripted responses, one per Stream() call.
type budgetModel struct {
	mu      sync.Mutex
	scripts []string
	calls   int
}

func (m *budgetModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func aiText(s string) messages.Message {
	m := messages.AI("")
	m.Content = s
	return m
}

// nudgeHuman builds the injected continuation message for assertions.
func isNudge(m messages.Message) bool {
	return m.Role == messages.RoleHuman && strings.Contains(m.Content, "token target")
}

func TestCheckTokenBudgetDecisionMatrix(t *testing.T) {
	// Disabled budget never continues.
	tr := tokenBudgetTracker{}
	if d := checkTokenBudget(&tr, 0, 1_000_000); d.Continue {
		t.Fatal("zero budget must disable auto-continue")
	}

	// Under 90%: continue, tracker advances.
	tr = tokenBudgetTracker{}
	d := checkTokenBudget(&tr, 1000, 100)
	if !d.Continue || !strings.Contains(d.Nudge, "10% of token target") {
		t.Fatalf("decision = %+v", d)
	}
	if tr.continuationCount != 1 || tr.lastTotalTokens != 100 {
		t.Fatalf("tracker = %+v", tr)
	}

	// At/over 90%: stop without diminishing.
	if d := checkTokenBudget(&tr, 1000, 900); d.Continue || d.Diminishing {
		t.Fatalf("budget reached must stop: %+v", d)
	}

	// Diminishing: 3+ continuations with both deltas under 500.
	tr = tokenBudgetTracker{continuationCount: 3, lastDeltaTokens: 100, lastTotalTokens: 1000}
	d = checkTokenBudget(&tr, 500_000, 1300) // delta 200 < 500
	if d.Continue || !d.Diminishing {
		t.Fatalf("diminishing returns must stop: %+v", d)
	}

	// Not diminishing when the current delta is large.
	tr = tokenBudgetTracker{continuationCount: 3, lastDeltaTokens: 100, lastTotalTokens: 1000}
	d = checkTokenBudget(&tr, 500_000, 2000) // delta 1000 >= 500
	if !d.Continue {
		t.Fatalf("large delta must continue: %+v", d)
	}

	// Not diminishing when the previous delta was large.
	tr = tokenBudgetTracker{continuationCount: 3, lastDeltaTokens: 900, lastTotalTokens: 1000}
	d = checkTokenBudget(&tr, 500_000, 1200)
	if !d.Continue {
		t.Fatalf("large previous delta must continue: %+v", d)
	}

	// Not diminishing before 3 continuations.
	tr = tokenBudgetTracker{continuationCount: 2, lastDeltaTokens: 10, lastTotalTokens: 100}
	d = checkTokenBudget(&tr, 500_000, 150)
	if !d.Continue {
		t.Fatalf("under 3 continuations must continue: %+v", d)
	}
}

func TestRunnerTokenBudgetContinuesUntilDiminishing(t *testing.T) {
	// Every round emits a tiny response (<500 tokens), so the diminishing
	// guard stops the run after exactly 3 continuations.
	model := &budgetModel{scripts: []string{
		strings.Repeat("round one output ", 5),
		strings.Repeat("round two output ", 5),
		strings.Repeat("round three output ", 5),
		strings.Repeat("round four output ", 5),
		strings.Repeat("round five output ", 5),
	}}
	a := newBudgetTestAgent(t, model)
	r := New(a, nil, nil)
	r.TokenBudget = 500_000

	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	// 1 original + 3 nudged rounds.
	if got := model.streamCalls(); got != 4 {
		t.Fatalf("model rounds = %d, want 4 (1 + 3 diminishing-capped continuations)", got)
	}
	nudges := 0
	for _, m := range out {
		if isNudge(m) {
			nudges++
		}
	}
	if nudges != 3 {
		t.Fatalf("nudge messages = %d, want 3", nudges)
	}
}

func TestRunnerTokenBudgetStopsWhenNearlySpent(t *testing.T) {
	// The first round alone covers >= 90% of a small budget: no nudge.
	big := strings.Repeat("substantial output ", 4000) // ~8000 tokens
	model := &budgetModel{scripts: []string{big, "should not run"}}
	a := newBudgetTestAgent(t, model)
	r := New(a, nil, nil)
	r.TokenBudget = 8_000

	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.streamCalls(); got != 1 {
		t.Fatalf("model rounds = %d, want 1 (budget nearly spent)", got)
	}
	for _, m := range out {
		if isNudge(m) {
			t.Fatal("nudge injected despite budget being nearly spent")
		}
	}
}

func TestRunnerTokenBudgetDisabledByDefault(t *testing.T) {
	model := &budgetModel{scripts: []string{"single answer"}}
	a := newBudgetTestAgent(t, model)
	r := New(a, nil, nil) // TokenBudget 0
	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.streamCalls(); got != 1 {
		t.Fatalf("model rounds = %d, want 1 (disabled)", got)
	}
	for _, m := range out {
		if isNudge(m) {
			t.Fatal("nudge injected while disabled")
		}
	}
}

func TestRunnerTokenBudgetLargeDeltasContinue(t *testing.T) {
	// Deltas stay >= 500 tokens: continuations proceed while under 90%.
	model := &budgetModel{scripts: []string{
		strings.Repeat("big round one ", 200), // ~400 tokens... make bigger
		strings.Repeat("big round two ", 200),
		strings.Repeat("big round three ", 200),
		strings.Repeat("big round four ", 200),
	}}
	// Each round must exceed the 500-token delta: 200 repeats * 3 words.
	a := newBudgetTestAgent(t, model)
	r := New(a, nil, nil)
	// Budget sized so three rounds stay under 90% but four exceed it.
	r.TokenBudget = 6_000

	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	rounds := model.streamCalls()
	if rounds < 3 {
		t.Fatalf("model rounds = %d, want >= 3 (large deltas must keep continuing)", rounds)
	}
	var nudges int
	for _, m := range out {
		if isNudge(m) {
			nudges++
		}
	}
	if nudges != rounds-1 {
		t.Fatalf("nudges = %d, rounds = %d (each continuation needs exactly one nudge)", nudges, rounds)
	}
}

// The ChatModel interface for budgetModel.

func (m *budgetModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return aiText("invoke unused"), nil
}

func (m *budgetModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}

func (m *budgetModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	i := m.calls
	m.calls++
	m.mu.Unlock()
	text := ""
	if i < len(m.scripts) {
		text = m.scripts[i]
	}
	return &budgetStream{chunk: aiText(text)}, nil
}

func (m *budgetModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *budgetModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *budgetModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *budgetModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

type budgetStream struct {
	chunk messages.Message
	done  bool
}

func (s *budgetStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.done {
		return messages.Message{}, false, nil
	}
	s.done = true
	return s.chunk, true, nil
}

func (s *budgetStream) Close() error { return nil }

// newBudgetTestAgent builds an agent around a budgetModel.
func newBudgetTestAgent(t *testing.T, model *budgetModel) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// turnsModel alternates tool-call and final responses across many rounds.
type turnsModel struct {
	mu    sync.Mutex
	calls int
}

func (m *turnsModel) round(i int) messages.Message {
	if i < 4 { // four consecutive tool rounds, then a natural finish
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: fmtID(i), Name: "ListDir", Args: map[string]any{"path": "."}},
		}}
	}
	return aiText(fmtIDText(i))
}

func fmtID(i int) string     { return "t" + strings.Repeat("x", i+1) }
func fmtIDText(i int) string { return "round " + strings.Repeat("y", i+1) + " done" }

func (m *turnsModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.AI("unused"), nil
}
func (m *turnsModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}
func (m *turnsModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	i := m.calls
	m.calls++
	m.mu.Unlock()
	return &budgetStream{chunk: m.round(i)}, nil
}
func (m *turnsModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *turnsModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *turnsModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *turnsModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

func TestRunnerMaxTurnsStopsAtLimit(t *testing.T) {
	model := &turnsModel{}
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		MaxTurns:       2,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)

	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	// 2 tools rounds => 2 model rounds done + the round that produced the
	// second tool call... each tools round consumes one model round, so the
	// model is called exactly MaxTurns times.
	if got := model.calls; got != 2 {
		t.Fatalf("model rounds = %d, want 2", got)
	}
	var marker bool
	for _, m := range out {
		if strings.Contains(m.Content, "<max-turns-reached>") {
			marker = true
		}
	}
	if !marker {
		t.Fatal("transcript missing the max-turns marker")
	}
}

func TestRunnerMaxTurnsUnlimitedByDefault(t *testing.T) {
	model := &turnsModel{}
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil) // MaxTurns 0
	out, err := r.Run(context.Background(), []messages.Message{messages.Human("work")})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if strings.Contains(m.Content, "<max-turns-reached>") {
			t.Fatal("marker present without a limit")
		}
	}
}

func TestHistoryFromAndTokenEstimate(t *testing.T) {
	msgs := []messages.Message{messages.System("s"), messages.Human("h")}
	if got := historyFrom(msgs); len(got) != 1 || got[0].Role != messages.RoleHuman {
		t.Errorf("historyFrom = %+v", got)
	}
	if got := historyFrom(nil); got != nil {
		t.Errorf("nil = %+v", got)
	}
	if got := historyFrom([]messages.Message{messages.Human("h")}); len(got) != 1 {
		t.Errorf("no system = %+v", got)
	}

	// AI messages with content and tool calls count; other roles do not.
	n := estimateTurnOutputTokens([]messages.Message{
		messages.Human("q"),
		messages.AI("answer"),
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{Name: "Read", Args: map[string]any{"file_path": "a.go"}}}},
	})
	if n <= 0 {
		t.Errorf("estimate = %d", n)
	}
	if got := estimateTurnOutputTokens([]messages.Message{messages.Human("q")}); got != 0 {
		t.Errorf("human-only estimate = %d", got)
	}
}
