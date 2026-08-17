package agent

import (
	"context"
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
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

// Aliases used by the tests below.
type (
	coreTool = coretools.Tool
	invOpt   = runnables.Option
	strm     = runnables.Stream[messages.Message]
	sch      = schema.Schema
	caps     = language.ChatModelCapabilities
)

var (
	schMap = schema.Schema{"type": "array"}
	schObj = schema.Object(map[string]schema.Schema{"role": schema.String("role")})
	capsV  = language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
)

// oneShot is a single-chunk stream.
type oneShot struct {
	chunk messages.Message
	done  bool
}

func (s *oneShot) Next(_ context.Context) (messages.Message, bool, error) {
	if s.done {
		return messages.Message{}, false, nil
	}
	s.done = true
	return s.chunk, true, nil
}

func (s *oneShot) Close() error { return nil }

// mustFromFunc builds a trivial echo tool by name.
func mustFromFunc(name string) coretools.Tool {
	t, err := coretools.FromFunc(name, "test tool",
		func(ctx context.Context, args map[string]any) (coretools.Result, error) {
			return coretools.Result{Content: "echo"}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// TestRefreshExtraToolsRebindsNewTools verifies the between-turn tool
// refresh: a changed extra set rebuilds the tool list and rebinds the model.
func TestRefreshExtraToolsRebindsNewTools(t *testing.T) {
	mk := func(name string) coreTool {
		return mustFromFunc(name)
	}
	model := &bindTrackingModel{}
	reg := tools.NewRegistry(t.TempDir())
	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		ExtraTools:     []coreTool{mk("extra_one")},
		RefreshTools: func(ctx context.Context) []coreTool {
			return []coreTool{mk("extra_one"), mk("extra_two")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.toolByName["extra_two"]; ok {
		t.Fatal("extra_two present before refresh")
	}
	boundBefore := model.bindCalls

	if !a.refreshExtraTools(context.Background()) {
		t.Fatal("refresh did not report a change")
	}
	if _, ok := a.toolByName["extra_two"]; !ok {
		t.Fatal("refreshed tool missing from the registry")
	}
	if model.bindCalls <= boundBefore {
		t.Fatal("model not rebound after the tool set changed")
	}
	// An unchanged set reports no change and does not rebind.
	boundNow := model.bindCalls
	if a.refreshExtraTools(context.Background()) {
		t.Fatal("unchanged set reported a change")
	}
	if model.bindCalls != boundNow {
		t.Fatal("model rebound for an unchanged set")
	}
}

// bindTrackingModel counts BindTools calls.
type bindTrackingModel struct {
	bindCalls int
}

func (m *bindTrackingModel) Invoke(ctx context.Context, input []messages.Message, opts ...invOpt) (messages.Message, error) {
	return messages.AI("ok"), nil
}
func (m *bindTrackingModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...invOpt) ([]messages.Message, error) {
	return nil, nil
}
func (m *bindTrackingModel) Stream(ctx context.Context, input []messages.Message, _ ...invOpt) (strm, error) {
	return &oneShot{chunk: messages.AI("ok")}, nil
}
func (m *bindTrackingModel) InputSchema() sch { return schMap }
func (m *bindTrackingModel) OutputSchema() sch {
	return schObj
}
func (m *bindTrackingModel) BindTools(bound []coreTool) (language.ChatModel, error) {
	m.bindCalls++
	return m, nil
}
func (m *bindTrackingModel) Capabilities() caps { return capsV }

// TestTaskSummaryPeriodicGeneration verifies the periodic task summary:
// generated every taskSummaryInterval rounds, stored, and readable.
func TestTaskSummaryPeriodicGeneration(t *testing.T) {
	summarizer := &countingSummarizer{reply: "Refactoring the auth middleware"}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
	)
	reg := tools.NewRegistry(t.TempDir())
	a, err := New(Config{
		Model:          model,
		Summarizer:     summarizer,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []messages.Message{messages.Human("refactor auth")}
	// Rounds below the interval: no generation yet.
	for i := 0; i < taskSummaryInterval-1; i++ {
		a.maybeBeginTaskSummary(context.Background(), msgs)
	}
	if summarizer.calls() != 0 {
		t.Fatalf("summarizer called %d times before the interval", summarizer.calls())
	}
	a.maybeBeginTaskSummary(context.Background(), msgs)
	deadline := time.Now().Add(3 * time.Second)
	for a.TaskSummary() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.TaskSummary(); got != "Refactoring the auth middleware" {
		t.Fatalf("task summary = %q", got)
	}
}

// countingSummarizer counts Invoke calls.
type countingSummarizer struct {
	mu    sync.Mutex
	n     int
	reply string
}

func (s *countingSummarizer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *countingSummarizer) Invoke(ctx context.Context, input []messages.Message, opts ...invOpt) (messages.Message, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return messages.AI(s.reply), nil
}
func (s *countingSummarizer) Batch(ctx context.Context, inputs [][]messages.Message, opts ...invOpt) ([]messages.Message, error) {
	return nil, nil
}
func (s *countingSummarizer) Stream(ctx context.Context, input []messages.Message, _ ...invOpt) (strm, error) {
	return &oneShot{chunk: messages.AI(s.reply)}, nil
}
func (s *countingSummarizer) InputSchema() sch { return schMap }
func (s *countingSummarizer) OutputSchema() sch {
	return schObj
}
func (s *countingSummarizer) BindTools(bound []coreTool) (language.ChatModel, error) {
	return s, nil
}
func (s *countingSummarizer) Capabilities() caps { return capsV }

// TestMemoryPrefetchAttachments verifies the memory attachments: entries
// matching the latest user message are prefetched and attached once.
func TestMemoryPrefetchAttachments(t *testing.T) {
	mem := store.NewMemory()
	ctx := context.Background()
	_ = mem.Put(ctx, "facts", "deploy-key", "the deploy uses bluegreen strategy")
	_ = mem.Put(ctx, "docs", "deploy-runbook", `"see runbook before every deploy"`)
	_ = mem.Put(ctx, "facts", "irrelevant", "completely unrelated content xyzzy")

	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true})),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Memory:         mem,
	})
	if err != nil {
		t.Fatal(err)
	}

	a.beginMemoryPrefetch(ctx, []messages.Message{messages.Human("how does the deploy work here")})
	deadline := time.Now().Add(3 * time.Second)
	var attached []messages.Message
	for time.Now().Before(deadline) {
		attached = a.takeMemoryAttachments(ctx)
		if len(attached) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(attached) != 2 {
		t.Fatalf("attachments = %d, want 2 (deploy facts + runbook): %+v", len(attached), attached)
	}
	joined := ""
	for _, m := range attached {
		joined += m.Content
	}
	if !strings.Contains(joined, "bluegreen") || !strings.Contains(joined, "see runbook before every deploy") {
		t.Fatalf("attachments missing matched memories:\n%s", joined)
	}
	if strings.Contains(joined, "unrelated") {
		t.Fatalf("irrelevant memory attached:\n%s", joined)
	}
	// Already-injected entries are not attached again.
	if again := a.takeMemoryAttachments(ctx); len(again) != 0 {
		t.Fatalf("duplicate attachments: %+v", again)
	}
}

func TestMemoryTerms(t *testing.T) {
	terms := memoryTerms([]messages.Message{messages.Human("Check the deploy config for auth service, please!")})
	want := map[string]bool{"check": false, "deploy": false, "config": false}
	for _, term := range terms {
		if _, ok := want[term]; ok {
			want[term] = true
		}
	}
	for term, seen := range want {
		if !seen {
			t.Fatalf("term %q missing from %v", term, terms)
		}
	}
	if len(memoryTerms(nil)) != 0 {
		t.Fatal("no human message must yield no terms")
	}
}
