package runner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/tools"
)

func TestRunnerInterruptAndRender(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, Content: "Let me write it.", ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("Done."),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	handled := 0
	var rendered strings.Builder
	r := New(a, func(value any) (any, error) {
		req, ok := value.(agent.PermissionRequest)
		if !ok {
			t.Fatalf("interrupt value = %T", value)
		}
		if req.ToolName != "Write" {
			t.Fatalf("unexpected tool %q", req.ToolName)
		}
		handled++
		return "y", nil
	}, DefaultRenderer(&rendered))

	out, err := r.Run(context.Background(), []messages.Message{messages.Human("write it")})
	if err != nil {
		t.Fatal(err)
	}
	if handled != 1 {
		t.Fatalf("interrupt handled %d times, want 1", handled)
	}
	if got := out[len(out)-1].Content; got != "Done." {
		t.Fatalf("final message = %q, want %q", got, "Done.")
	}
	if !strings.Contains(rendered.String(), "Let me write it.") {
		t.Fatalf("render missing model text:\n%s", rendered.String())
	}
	if !strings.Contains(rendered.String(), "Write(") {
		t.Fatalf("render missing tool call:\n%s", rendered.String())
	}
}

// retainEngine is a minimal memory.Engine capturing Remember/RecordEvent for
// the auto-retain wiring test (the agent-package fakeEngine is not exported).
type retainEngine struct {
	mu       sync.Mutex
	remember []string
	events   []string
	eventObs []string
}

func (e *retainEngine) Remember(_ context.Context, _, content string, _ []string, _ string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remember = append(e.remember, content)
	return "id-1", nil
}

func (e *retainEngine) RecordEvent(_ context.Context, _, agentName, action, observation, outcome string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, agentName+"/"+action+"/"+outcome)
	e.eventObs = append(e.eventObs, observation)
	return nil
}

func (e *retainEngine) Remembered() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.remember...)
}

func (e *retainEngine) Events() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func (e *retainEngine) EventObs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.eventObs...)
}

func (e *retainEngine) Recall(context.Context, string, string, int) ([]memory.Hit, error) {
	return nil, nil
}
func (e *retainEngine) SearchCode(context.Context, string, string, int) ([]memory.Hit, error) {
	return nil, nil
}
func (e *retainEngine) List(context.Context, string, string, int, int) ([]memory.Hit, error) {
	return nil, nil
}
func (e *retainEngine) Forget(context.Context, string) error { return nil }
func (e *retainEngine) Stats(context.Context, string) (*memory.Stats, error) {
	return &memory.Stats{Total: 0, Layers: map[string]int{}}, nil
}
func (e *retainEngine) IndexCode(context.Context, string, string, bool) (string, error) {
	return "", nil
}
func (e *retainEngine) Consolidate(context.Context, string, float64) (string, error) { return "", nil }
func (e *retainEngine) Description() string                                          { return "retain-stub" }
func (e *retainEngine) Close() error                                                 { return nil }

// TestRunnerRetainsRunIntoMemory checks the runner-level wiring: with a
// memory engine configured (and settings defaults, autoretain on), Run's
// completion hook stores the run's Q/A and outcome.
func TestRunnerRetainsRunIntoMemory(t *testing.T) {
	eng := &retainEngine{}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("stored answer")),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		MemoryEngine:   eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	if _, err := r.Run(context.Background(), []messages.Message{messages.Human("remember me")}); err != nil {
		t.Fatal(err)
	}
	remembered := eng.Remembered()
	if len(remembered) != 0 {
		t.Fatalf("auto-retain must not write facts directly: %v", remembered)
	}
	if ev := eng.Events(); len(ev) != 1 || ev[0] != "woca/run/success" {
		t.Fatalf("events = %v", ev)
	}
	if obs := eng.EventObs(); len(obs) != 1 || !strings.Contains(obs[0], "Q: remember me") ||
		!strings.Contains(obs[0], "A: stored answer") {
		t.Fatalf("episode observations = %q", obs)
	}
}

// TestRunGoalRetainsGoalIntoMemory checks the goal-mode wiring: goal runs
// bypass Run, so RunGoal's completion hook must be the one storing the goal's
// objective, terminal status, and evidence.
func TestRunGoalRetainsGoalIntoMemory(t *testing.T) {
	eng := &retainEngine{}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("done-criteria checklist\nThis cycle: implement it"),             // assess
			messages.AI("implemented and verified"),                                      // execute
			messages.AI(`{"verdict":"complete","confidence":0.9,"evidence":"3/3 pass"}`), // evaluate
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 50,
		MemoryEngine:   eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGoal("make the tests pass", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	if _, err := r.RunGoal(context.Background(), []messages.Message{messages.Human("hello")}); err != nil {
		t.Fatal(err)
	}
	if ev := eng.Events(); len(ev) != 1 || ev[0] != "woca/goal:complete/success" {
		t.Fatalf("events = %v", ev)
	}
	if remembered := eng.Remembered(); len(remembered) != 0 {
		t.Fatalf("auto-retain must not write facts directly: %v", remembered)
	}
	if obs := eng.EventObs(); len(obs) != 1 ||
		!strings.Contains(obs[0], "GOAL: make the tests pass") ||
		!strings.Contains(obs[0], "RESULT(complete): 3/3 pass") {
		t.Fatalf("episode observations = %q", obs)
	}
}

// TestRunGoalRefusedStartRetainsNothing checks the started guard: a RunGoal
// call refused because the goal is paused must not re-record the goal.
func TestRunGoalRefusedStartRetainsNothing(t *testing.T) {
	eng := &retainEngine{}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		MemoryEngine:   eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGoal("objective", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	a.PauseGoal()
	r := New(a, nil, nil)
	if _, err := r.RunGoal(context.Background(), nil); err == nil {
		t.Fatal("RunGoal should refuse a paused goal")
	}
	if ev := eng.Events(); len(ev) != 0 || len(eng.Remembered()) != 0 {
		t.Fatalf("refused start must retain nothing: events=%v remembered=%v", eng.Events(), eng.Remembered())
	}
}

func TestRunnerStopHookContinuation(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("first"), messages.AI("continued")),
	)
	stops := 0
	hr := hooks.NewRunner(
		hooks.Config{Stop: []hooks.Entry{{Matcher: "*", Hooks: []hooks.Hook{{Command: "stop"}}}}},
		"/tmp", "s1",
		hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			stops++
			if stops == 1 {
				return hooks.CommandResult{Stdout: "keep going", ExitCode: 2}, nil
			}
			return hooks.CommandResult{}, nil
		}),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Hooks:          hr,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	out, err := r.Run(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[len(out)-1].Content; got != "continued" {
		t.Fatalf("final = %q, want continued", got)
	}
	if stops < 2 {
		t.Fatalf("stop hook should run at least twice, got %d", stops)
	}
}

func TestRunnerOutputLimitRecovery(t *testing.T) {
	truncated := messages.AI("partial output...")
	truncated.ResponseMetadata = map[string]any{"stop_reason": "max_tokens"}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			truncated,
			messages.AI("resumed and complete."),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	out, err := r.Run(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	var sawRecovery bool
	for _, m := range out {
		if m.Role == messages.RoleHuman && m.Content == agent.OutputLimitRecoveryPrompt {
			sawRecovery = true
		}
	}
	if !sawRecovery {
		t.Fatal("streaming recovery did not inject the continuation message")
	}
	if last := out[len(out)-1]; last.Content != "resumed and complete." {
		t.Fatalf("final = %+v", last)
	}
}

func TestRunnerOutputLimitEscalation(t *testing.T) {
	truncated := messages.AI("cut at 8k")
	truncated.ResponseMetadata = map[string]any{"stop_reason": "max_tokens"}
	primary := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(truncated),
	)
	escalated := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("full at 64k")),
	)
	escCalls := 0
	a, err := agent.New(agent.Config{
		Model:          primary,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		MaxTokensEscalator: func() (language.ChatModel, error) {
			escCalls++
			return escalated, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	out, err := r.Run(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	if escCalls != 1 {
		t.Fatalf("escalator called %d times, want 1", escCalls)
	}
	for _, m := range out {
		if m.Role == messages.RoleHuman && m.Content == agent.OutputLimitRecoveryPrompt {
			t.Fatal("escalation path must not inject a recovery prompt")
		}
	}
	if last := out[len(out)-1]; last.Content != "full at 64k" {
		t.Fatalf("final = %+v", last)
	}
	// The original model is restored after the turn (behind the agent's
	// observing wrapper — unwrap before comparing).
	base := a.CurrentModel()
	for {
		u, ok := base.(interface{ Unwrap() language.ChatModel })
		if !ok || u.Unwrap() == nil {
			break
		}
		base = u.Unwrap()
	}
	if base != primary {
		t.Fatal("original model not restored after escalation")
	}
}

// stubScript is one Stream() call: chunks, or a failure.
type stubScript struct {
	chunks []messages.Message
	fail   error
}

// stubModel plays scripts in order (Invoke returns the invoke text, serving
// as the compaction summarizer).
type stubModel struct {
	mu      sync.Mutex
	scripts []stubScript
	calls   int
	invoke  string
}

func (m *stubModel) streamCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *stubModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	if m.invoke == "" {
		return messages.AI("summary of the conversation"), nil
	}
	return messages.AI(m.invoke), nil
}

func (m *stubModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, errors.New("batch not used")
}

func (m *stubModel) Stream(ctx context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.mu.Lock()
	i := m.calls
	m.calls++
	m.mu.Unlock()
	script := stubScript{}
	if i < len(m.scripts) {
		script = m.scripts[i]
	}
	return &stubStream{script: script}, nil
}

func (m *stubModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *stubModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *stubModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *stubModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

type stubStream struct {
	script stubScript
	i      int
	failed bool
}

func (s *stubStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.script.fail != nil && !s.failed {
		s.failed = true
		return messages.Message{}, false, s.script.fail
	}
	if s.i >= len(s.script.chunks) {
		return messages.Message{}, false, nil
	}
	chunk := s.script.chunks[s.i]
	s.i++
	return chunk, true, nil
}

func (s *stubStream) Close() error { return nil }

func TestRunnerPromptTooLongRecovery(t *testing.T) {
	ptl := errors.New("prompt is too long: 137500 tokens > 135000 maximum")
	model := &stubModel{
		scripts: []stubScript{
			{fail: ptl},
			{chunks: []messages.Message{messages.AI("recovered")}},
		},
		invoke: "summary of the conversation",
	}
	a, err := agent.New(agent.Config{
		Model:          model,
		Summarizer:     model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	history := []messages.Message{messages.Human("start")}
	for i := 0; i < 6; i++ {
		history = append(history,
			messages.AI(strings.Repeat("analysis ", 30)),
			messages.Human(strings.Repeat("question ", 30)),
		)
	}
	out, err := r.Run(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	if last := out[len(out)-1]; last.Content != "recovered" {
		t.Fatalf("final = %+v", last)
	}
	var sawSummary bool
	for _, m := range out {
		if m.Role == messages.RoleSystem && strings.Contains(m.Content, "summary of the conversation") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("streaming recovery missing the compaction summary")
	}
}

func TestRunnerConsumesToolUseSummaries(t *testing.T) {
	// Slow summarizer: the summary settles only after the turn's model
	// stream, so the runner's bounded end-of-turn wait must pick it up.
	summarizer := &slowSummaryModel{delay: 120 * time.Millisecond}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "ListDir", Args: map[string]any{"path": "."}},
			}},
			messages.AI("done"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:                model,
		Summarizer:           summarizer,
		Registry:             tools.NewRegistry(t.TempDir()),
		SystemPrompt:         "test",
		PermissionMode:       config.ModeBypass,
		EmitToolUseSummaries: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(a, nil, nil)
	var mu sync.Mutex
	var rendered []agent.ToolUseSummary
	r.RenderSummary = func(s agent.ToolUseSummary) {
		mu.Lock()
		defer mu.Unlock()
		rendered = append(rendered, s)
	}
	if _, err := r.Run(context.Background(), []messages.Message{messages.Human("list")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rendered) != 1 {
		t.Fatalf("rendered summaries = %d, want 1", len(rendered))
	}
	if rendered[0].Text != "Listed directory" || len(rendered[0].ToolCallIDs) != 1 || rendered[0].ToolCallIDs[0] != "1" {
		t.Fatalf("summary = %+v", rendered[0])
	}
}

// slowSummaryModel delays its Invoke to model an in-flight summary.
type slowSummaryModel struct {
	mu    sync.Mutex
	delay time.Duration
}

func (m *slowSummaryModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
		return messages.Message{}, ctx.Err()
	}
	return messages.AI("Listed directory"), nil
}

func (m *slowSummaryModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}
func (m *slowSummaryModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &oneShotStream{chunk: messages.AI("x")}, nil
}
func (m *slowSummaryModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *slowSummaryModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (m *slowSummaryModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *slowSummaryModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{Streaming: true}
}

type oneShotStream struct {
	chunk messages.Message
	done  bool
}

func (s *oneShotStream) Next(_ context.Context) (messages.Message, bool, error) {
	if s.done {
		return messages.Message{}, false, nil
	}
	s.done = true
	return s.chunk, true, nil
}
func (s *oneShotStream) Close() error { return nil }

func TestSummarizeHelpers(t *testing.T) {
	if got := summarizeArgs(nil); got != "" {
		t.Errorf("nil args = %q", got)
	}
	if got := summarizeArgs(map[string]any{"a": "b"}); got != `{"a":"b"}` {
		t.Errorf("args = %q", got)
	}
	long := map[string]any{"k": strings.Repeat("x", 300)}
	if got := summarizeArgs(long); len(got) <= 160 || !strings.HasSuffix(got, "…") {
		t.Errorf("long args = %q", got)
	}
	if got := summarizeResult(""); got != "(no output)" {
		t.Errorf("empty result = %q", got)
	}
	if got := summarizeResult("  first\nsecond  "); got != "first" {
		t.Errorf("multiline = %q", got)
	}
	if got := summarizeResult(strings.Repeat("y", 250)); len(got) <= 200 {
		t.Errorf("long result = %q", got)
	}
	// The summary renderer prints the text line.
	var buf bytes.Buffer
	DefaultSummaryRenderer(&buf)(agent.ToolUseSummary{Text: "used Bash"})
	if !strings.Contains(buf.String(), "· used Bash") {
		t.Errorf("summary render = %q", buf.String())
	}
	DefaultSummaryRenderer(&buf)(agent.ToolUseSummary{Text: ""})
}

// The renderer's non-delta branches (tool call / result chunks) execute
// even when unused by other tests.
func TestDefaultRendererToolSteps(t *testing.T) {
	var buf bytes.Buffer
	r := DefaultRenderer(&buf)
	r(lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"tools": map[string]any{}}})
	r(lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": map[string]any{
		"messages": []messages.Message{
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{Name: "Read", Args: map[string]any{"file_path": "a.go"}}}},
			messages.Tool("1", "file body"),
		},
	}}})
	if !strings.Contains(buf.String(), "Read(") || !strings.Contains(buf.String(), "file body") {
		t.Fatalf("tool chunks not rendered:\n%s", buf.String())
	}
}
