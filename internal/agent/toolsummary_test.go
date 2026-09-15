package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// slowSummarizer sleeps before answering, recording the prompts it saw.
type slowSummarizer struct {
	mu      sync.Mutex
	prompts []string
	delay   time.Duration
	fail    bool
}

func (s *slowSummarizer) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	var b strings.Builder
	for _, m := range input {
		b.WriteString(string(m.Role) + ": " + m.Content + "\n")
	}
	s.mu.Lock()
	s.prompts = append(s.prompts, b.String())
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return messages.Message{}, ctx.Err()
		}
	}
	if s.fail {
		return messages.Message{}, context.DeadlineExceeded
	}
	return messages.AI("Read config.json"), nil
}

func (s *slowSummarizer) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	return nil, nil
}
func (s *slowSummarizer) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &singleChunkStream{chunk: messages.AI("Read config.json")}, nil
}
func (s *slowSummarizer) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (s *slowSummarizer) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}
func (s *slowSummarizer) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return s, nil
}
func (s *slowSummarizer) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{Streaming: true}
}

func (s *slowSummarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

func newSummaryTestAgent(t *testing.T, summarizer language.ChatModel, emit bool) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, Content: "checking the config", ToolCalls: []messages.ToolCall{
				{ID: "s1", Name: "Read", Args: map[string]any{"file_path": "config.json"}},
			}},
			messages.AI("finished the task"),
		),
	)
	dir := t.TempDir()
	if err := writeFileForSummaryTest(dir+"/config.json", "{}"); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Model:                model,
		Summarizer:           summarizer,
		Registry:             tools.NewRegistry(dir),
		SystemPrompt:         "test",
		PermissionMode:       config.ModeBypass,
		EmitToolUseSummaries: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeFileForSummaryTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// awaitSummaries waits (bounded) until at least n summaries settle and are
// consumable.
func awaitSummaries(t *testing.T, a *Agent, n int) []ToolUseSummary {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		summ := takeAll(t, a)
		if len(summ) >= n {
			return summ
		}
		if time.Now().After(deadline) {
			t.Fatalf("summaries did not settle within deadline (pending=%d)", a.PendingToolUseSummaryCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func takeAll(t *testing.T, a *Agent) []ToolUseSummary {
	t.Helper()
	wctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	return a.WaitToolUseSummaries(wctx)
}

func TestToolUseSummaryGeneratedAsync(t *testing.T) {
	summarizer := &slowSummarizer{delay: 200 * time.Millisecond}
	a := newSummaryTestAgent(t, summarizer, true)

	start := time.Now()
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("check config")}, "t")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// Generation is async: the turn must not wait for the 200ms summarizer.
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Invoke blocked on the summarizer: %v", elapsed)
	}
	// The summary eventually settles with the label and the batch's IDs.
	summs := awaitSummaries(t, a, 1)
	if len(summs) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summs))
	}
	s := summs[0]
	if s.Text != "Read config.json" {
		t.Fatalf("summary text = %q", s.Text)
	}
	if len(s.ToolCallIDs) != 1 || s.ToolCallIDs[0] != "s1" {
		t.Fatalf("summary IDs = %v", s.ToolCallIDs)
	}
	// The summarizer prompt carries the tool info and the intent text.
	summarizer.mu.Lock()
	prompt := ""
	if len(summarizer.prompts) > 0 {
		prompt = summarizer.prompts[0]
	}
	summarizer.mu.Unlock()
	if !strings.Contains(prompt, "Read") || !strings.Contains(prompt, "config.json") ||
		!strings.Contains(prompt, "checking the config") {
		t.Fatalf("summarizer prompt missing tool/intent info:\n%s", prompt)
	}
	// The summary is UI-only: it never enters the transcript as the final AI
	// message (the final AI is "finished the task").
	if last := out[len(out)-1]; last.Content != "finished the task" {
		t.Fatalf("final message = %+v", last)
	}
}

func TestToolUseSummaryDisabled(t *testing.T) {
	summarizer := &slowSummarizer{}
	a := newSummaryTestAgent(t, summarizer, false)
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("check config")}, "t"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := summarizer.callCount(); got != 0 {
		t.Fatalf("summarizer called %d times while disabled", got)
	}
	if got := a.PendingToolUseSummaryCount(); got != 0 {
		t.Fatalf("pending summaries = %d while disabled", got)
	}
}

func TestToolUseSummarySubagentSkipped(t *testing.T) {
	summarizer := &slowSummarizer{}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "sa1", Name: "Read", Args: map[string]any{"file_path": "config.json"}},
			}},
			messages.AI("done"),
		),
	)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/config.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Model:                model,
		Summarizer:           summarizer,
		Registry:             tools.NewRegistry(dir),
		SystemPrompt:         "test",
		PermissionMode:       config.ModeBypass,
		Subagent:             true,
		EmitToolUseSummaries: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("check")}, "t"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := summarizer.callCount(); got != 0 {
		t.Fatalf("subagent batches must not generate summaries, got %d calls", got)
	}
}

func TestToolUseSummaryFailureSilent(t *testing.T) {
	summarizer := &slowSummarizer{fail: true, delay: 20 * time.Millisecond}
	a := newSummaryTestAgent(t, summarizer, true)
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("check config")}, "t")
	if err != nil {
		t.Fatalf("summarizer failure must not fail the run: %v", err)
	}
	if last := out[len(out)-1]; last.Content != "finished the task" {
		t.Fatalf("final message = %+v", last)
	}
	// The failed summary settles to nothing: nothing consumable, no error.
	time.Sleep(100 * time.Millisecond)
	if got := takeAll(t, a); len(got) != 0 {
		t.Fatalf("failed summary produced output: %+v", got)
	}
}

func TestTakeReadyNonBlockingSemantics(t *testing.T) {
	summarizer := &slowSummarizer{delay: 10 * time.Millisecond}
	a := newSummaryTestAgent(t, summarizer, true)
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("check config")}, "t"); err != nil {
		t.Fatal(err)
	}
	// TakeReady never blocks: immediately after the turn it may legitimately
	// be empty while generation is in flight.
	_ = a.TakeReadyToolUseSummaries()
	// Once settled, exactly one summary is returned once (second take empty).
	summs := awaitSummaries(t, a, 1)
	if len(summs) != 1 {
		t.Fatalf("summaries = %d", len(summs))
	}
	if again := a.TakeReadyToolUseSummaries(); len(again) != 0 {
		t.Fatalf("consumed summary returned again: %+v", again)
	}
}
