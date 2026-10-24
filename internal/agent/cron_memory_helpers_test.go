package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

// --- cron.go ---

func TestCronSchedulerStartStopLifecycle(t *testing.T) {
	fired := false
	s := newCronScheduler(func(tag, text string) { fired = true })
	s.Start()
	s.Start() // idempotent: no second loop
	s.Stop()
	s.Stop() // nil-safe
	if fired {
		t.Fatal("no tick should fire during the test")
	}
}

func TestCronFireDueDisplayNameAndDedup(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	a.cron = newCronScheduler(func(tag, text string) {
		mu.Lock()
		texts = append(texts, tag+"|"+text)
		mu.Unlock()
	})
	// A named entry renders its name; an unnamed one its id.
	if _, err := a.addCron("daily", "do the thing", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.addCron("", "other thing", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 9, 30, 0, 0, time.Local)
	a.fireDueCron(now)
	a.fireDueCron(now.Add(20 * time.Second)) // same minute: suppressed
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 2 {
		t.Fatalf("fired %d notifications, want 2: %v", len(texts), texts)
	}
	if !strings.Contains(texts[0], "daily") || !strings.Contains(texts[0], "scheduled-task") {
		t.Fatalf("named fire = %q", texts[0])
	}
	if !strings.Contains(texts[1], "cron-2") {
		t.Fatalf("unnamed fire = %q", texts[1])
	}
}

func TestScheduleCronToolPaths(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	defer a.stopCron()

	// Empty list.
	if res := a.execute(t.Context(), "ScheduleCron", map[string]any{"list": true}); res.Content != "(no scheduled entries)" {
		t.Fatalf("empty list = %q", res.Content)
	}
	// Create requires prompt and cron.
	if res := a.execute(t.Context(), "ScheduleCron", map[string]any{"prompt": "p"}); !strings.Contains(res.Content, "requires prompt and cron") {
		t.Fatalf("missing cron = %q", res.Content)
	}
	// Invalid spec surfaces the parser error.
	if res := a.execute(t.Context(), "ScheduleCron", map[string]any{"prompt": "p", "cron": "bad"}); !strings.Contains(res.Content, "Error:") {
		t.Fatalf("bad cron = %q", res.Content)
	}
	// Create + list + delete round-trip.
	res := a.execute(t.Context(), "ScheduleCron", map[string]any{"name": "tick", "prompt": "first line\nsecond line", "cron": "* * * * *"})
	if !strings.Contains(res.Content, "Scheduled cron-1") {
		t.Fatalf("create = %q", res.Content)
	}
	res = a.execute(t.Context(), "ScheduleCron", map[string]any{"list": true})
	if !strings.Contains(res.Content, "cron-1") || !strings.Contains(res.Content, "tick: first line") {
		t.Fatalf("list = %q", res.Content)
	}
	if res := a.execute(t.Context(), "ScheduleCron", map[string]any{"delete_id": "cron-1"}); !strings.Contains(res.Content, "Deleted cron-1") {
		t.Fatalf("delete = %q", res.Content)
	}
	if res := a.execute(t.Context(), "ScheduleCron", map[string]any{"delete_id": "cron-1"}); !strings.Contains(res.Content, "no scheduled entry") {
		t.Fatalf("double delete = %q", res.Content)
	}
}

func TestCronFieldParseErrors(t *testing.T) {
	for _, bad := range []string{
		"*/0 * * * *",  // zero step
		"*/x * * * *",  // non-numeric step
		"a-b * * * *",  // bad range
		"1-99 * * * *", // range beyond max
		"5-3 * * * *",  // inverted range
	} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("parseCron(%q) should fail", bad)
		}
	}
}

func TestPreviewCronAndText(t *testing.T) {
	if got := previewCron(cronEntry{Name: "n", Prompt: "p"}); got != "n: p" {
		t.Fatalf("named preview = %q", got)
	}
	if got := previewCron(cronEntry{Prompt: "p"}); got != "p" {
		t.Fatalf("unnamed preview = %q", got)
	}
	if got := previewText("  line1\nline2  ", 40); got != "line1" {
		t.Fatalf("multiline preview = %q", got)
	}
	if got := previewText(strings.Repeat("a", 50), 10); got != "aaaaaaaaaa…" {
		t.Fatalf("long preview = %q", got)
	}
}

// --- filehistory.go ---

func TestFileHistoryDelegates(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	ctx := t.Context()
	if a.FileHistory() == nil {
		t.Fatal("registry-backed agent should expose FileHistory")
	}
	// Record one mutation and count it.
	path := a.registry.Dir().Base() + "/fh.txt"
	a.FileHistory().Record(ctx, path, nil, []byte("after"))
	if undo, redo := a.FileHistoryCounts(ctx); undo != 1 || redo != 0 {
		t.Fatalf("counts = %d/%d", undo, redo)
	}
	msg, err := a.UndoFiles(ctx)
	if err != nil || !strings.Contains(msg, "Undid") {
		t.Fatalf("undo = %q, %v", msg, err)
	}
	if _, redo := a.FileHistoryCounts(ctx); redo != 1 {
		t.Fatalf("redo count = %d", redo)
	}
	msg, err = a.RedoFiles(ctx)
	if err != nil || !strings.Contains(msg, "Redid") {
		t.Fatalf("redo = %q, %v", msg, err)
	}

	// A bare agent (no registry) reports unavailability without panicking.
	bare := &Agent{}
	if undo, redo := bare.FileHistoryCounts(ctx); undo != 0 || redo != 0 {
		t.Fatalf("bare counts = %d/%d", undo, redo)
	}
	if _, err := bare.UndoFiles(ctx); err == nil {
		t.Fatal("bare UndoFiles must error")
	}
	if _, err := bare.RedoFiles(ctx); err == nil {
		t.Fatal("bare RedoFiles must error")
	}
	if bare.FileHistory() != nil {
		t.Fatal("bare FileHistory must be nil")
	}
}

func TestCloseAndTeamStatusText(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if txt, err := a.TeamStatusText(); err != nil || !strings.Contains(txt, "no team spawned") {
		t.Fatalf("TeamStatusText = %q, %v", txt, err)
	}
	// With a team: the status renders the (empty) member list.
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	lead.ensureTeam()
	if txt, err := lead.TeamStatusText(); err != nil || !strings.Contains(txt, "(no teammates spawned yet)") {
		t.Fatalf("team TeamStatusText = %q, %v", txt, err)
	}
	// Close is safe with and without a team.
	lead.Close()
	a.Close()
}

// --- memorytools.go ---

// stubEngine extends fakeEngine with injectable SearchCode/IndexCode results.
type stubEngine struct {
	*fakeEngine
	searchErr  error
	indexErr   error
	indexCalls []string
}

func (s *stubEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return s.fakeEngine.SearchCode(ctx, q, ws, k)
}

func (s *stubEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	s.mu.Lock()
	s.indexCalls = append(s.indexCalls, root)
	s.mu.Unlock()
	if s.indexErr != nil {
		return "", s.indexErr
	}
	return "indexed 3 files", nil
}

func TestMemoryToolsErrorAndDisabledPaths(t *testing.T) {
	f := &stubEngine{fakeEngine: &fakeEngine{}}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	byName := toolsByName(a.memoryTools())

	// Engine errors surface as tool errors.
	f.searchErr = errors.New("search boom")
	if _, err := byName["search_code"].Invoke(t.Context(), map[string]any{"query": "x"}); err == nil {
		t.Fatal("search_code error must propagate")
	}
	f.searchErr = nil
	f.err = true
	if _, err := byName["recall_memory"].Invoke(t.Context(), map[string]any{"query": "x"}); err == nil {
		t.Fatal("recall_memory error must propagate")
	}
	f.err = false
	f.rememberErr = errors.New("remember boom")
	if _, err := byName["remember_fact"].Invoke(t.Context(), map[string]any{"content": "c"}); err == nil {
		t.Fatal("remember_fact error must propagate")
	}
	f.rememberErr = nil
	f.indexErr = errors.New("index boom")
	if _, err := byName["index_code"].Invoke(t.Context(), map[string]any{"root": "/src"}); err == nil {
		t.Fatal("index_code error must propagate")
	}
	f.indexErr = nil

	// index_code defaults the root to the agent workdir and returns the report.
	res, err := byName["index_code"].Invoke(t.Context(), map[string]any{})
	if err != nil || res.Content != "indexed 3 files" {
		t.Fatalf("index_code = %q, %v", res.Content, err)
	}
	f.mu.Lock()
	roots := append([]string(nil), f.indexCalls...)
	f.mu.Unlock()
	if len(roots) == 0 || roots[len(roots)-1] != "/ws" {
		t.Fatalf("index roots = %v", roots)
	}

	// A nil engine after the tools were built: every tool degrades to a note.
	a.mem.SetEngine(nil)
	for _, name := range []string{"recall_memory", "search_code", "remember_fact", "index_code"} {
		res, err := byName[name].Invoke(t.Context(), map[string]any{"query": "x", "content": "c"})
		if err != nil || res.Content != "memory disabled" {
			t.Fatalf("%s with nil engine = %q, %v", name, res.Content, err)
		}
	}
}

// --- memorystore.go ---

func TestMemoryRuntimeSetEngineAndGuards(t *testing.T) {
	m := memoryRuntime{}
	if m.Engine() != nil {
		t.Fatal("fresh runtime has no engine")
	}
	f := &fakeEngine{}
	m.SetEngine(f)
	if m.Engine() != f {
		t.Fatal("SetEngine did not swap")
	}
	// Begin without an engine or store is a no-op.
	m.Begin(t.Context(), []messages.Message{messages.Human("find the auth token")}, "/ws")
	// Begin with an engine but no human query is a no-op.
	m.SetEngine(&fakeEngine{})
	m.Begin(t.Context(), nil, "/ws")
}

func TestMemoryBeginKVPathAndAttachments(t *testing.T) {
	kv := store.NewMemory()
	ctx := t.Context()
	if err := kv.Put(ctx, "ns", "authkey", "holds the auth token"); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "ns", "empty", ""); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("y", 3000)
	if err := kv.Put(ctx, "ns", "longkey", long); err != nil {
		t.Fatal(err)
	}
	a := &Agent{workdir: "/ws"}
	a.mem.init(kv, nil, nil, nil)

	a.mem.Begin(ctx, []messages.Message{messages.Human("please find the auth token")}, "/ws")
	waitFor(t, 5*time.Second, func() bool {
		a.mem.mu.Lock()
		defer a.mem.mu.Unlock()
		return len(a.mem.prefetch.entries) > 0
	}, "KV prefetch settle")

	first := a.mem.TakeAttachments(ctx)
	found := false
	for _, m := range first {
		if strings.Contains(m.Content, "ns/authkey") && strings.Contains(m.Content, "auth token") {
			found = true
		}
	}
	if !found {
		t.Fatalf("attachments missing the auth entry: %+v", first)
	}
	// Second take is empty: the injected set deduplicates.
	if again := a.mem.TakeAttachments(ctx); len(again) != 0 {
		t.Fatalf("duplicate attachments: %+v", again)
	}

	// A fresh runtime with the same store skips empty values and clamps long ones.
	b := &Agent{workdir: "/ws"}
	b.mem.init(kv, nil, nil, nil)
	b.mem.Begin(ctx, []messages.Message{messages.Human("the long value please")}, "/ws")
	waitFor(t, 5*time.Second, func() bool {
		b.mem.mu.Lock()
		defer b.mem.mu.Unlock()
		return len(b.mem.prefetch.entries) > 0
	}, "second KV prefetch settle")
	for _, m := range b.mem.TakeAttachments(t.Context()) {
		if strings.Contains(m.Content, "ns/empty\n\n") {
			t.Fatalf("empty entry attached: %q", m.Content)
		}
		if len(m.Content) > 2100 {
			t.Fatalf("long entry not clamped: %d chars", len(m.Content))
		}
	}
}

func TestTakeAttachmentsBoundsAndReset(t *testing.T) {
	m := memoryRuntime{}
	m.mu.Lock()
	var hits []memory.Hit
	for i := range 7 {
		hits = append(hits, memory.Hit{ID: string(rune('a' + i)), Layer: "L2", Type: "fact", Summary: "s"})
	}
	// One hit with neither summary nor content is skipped.
	hits = append(hits, memory.Hit{ID: "blank", Layer: "L2", Type: "fact"})
	m.prefetch.engineHits = hits
	m.mu.Unlock()
	got := m.TakeAttachments(t.Context())
	if len(got) != 5 {
		t.Fatalf("attachments = %d, want the max of 5", len(got))
	}
	// The injected set resets past its bound instead of growing unbounded.
	m.mu.Lock()
	injected := map[string]bool{}
	for i := range injectedMemoryLimit + 10 {
		injected[string(rune(i%128))+"x"+string(rune(i))] = true
	}
	m.prefetch.injected = injected
	m.prefetch.engineHits = []memory.Hit{{ID: "fresh", Layer: "L2", Type: "fact", Summary: "s"}}
	m.mu.Unlock()
	if out := m.TakeAttachments(t.Context()); len(out) != 1 {
		t.Fatalf("post-reset attachments = %d", len(out))
	}
	m.mu.Lock()
	size := len(m.prefetch.injected)
	m.mu.Unlock()
	if size > injectedMemoryLimit+1 {
		t.Fatalf("injected set = %d, want reset near the bound", size)
	}
}

func TestMemoryStringHelpers(t *testing.T) {
	if got := trimJSONString([]byte(`"quoted"`)); got != "quoted" {
		t.Fatalf("trimJSONString = %q", got)
	}
	if got := trimJSONString([]byte(`"unterminated`)); got != `"unterminated` {
		t.Fatalf("bad JSON passthrough = %q", got)
	}
	if got := trimJSONString([]byte("plain")); got != "plain" {
		t.Fatalf("plain = %q", got)
	}
	if got := clampUTF8("héllo", -1); got != "" {
		t.Fatalf("negative clamp = %q", got)
	}
	if got := clampUTF8("hello", 3); got != "hel" {
		t.Fatalf("byte clamp = %q", got)
	}
	if got := clampUTF8("héllo", 3); !utf8.ValidString(got) || got == "" || len(got) > 3 {
		t.Fatalf("rune-boundary clamp = %q", got)
	}
	if got := lastHumanQuery(nil); got != "" {
		t.Fatalf("lastHumanQuery(nil) = %q", got)
	}
	if got := memoryTerms(nil); got != nil {
		t.Fatalf("memoryTerms(nil) = %v", got)
	}
	terms := memoryTerms([]messages.Message{messages.Human("Find THE auth token auth")})
	counts := map[string]int{}
	for _, term := range terms {
		counts[term]++
	}
	if counts["auth"] != 1 || !seenTerm(terms, "token") || seenTerm(terms, "the") {
		t.Fatalf("terms = %v", terms)
	}
}

func seenTerm(terms []string, want string) bool {
	return slices.Contains(terms, want)
}

// --- retain.go ---

func TestRetainRunAndGoalPaths(t *testing.T) {
	ctx := t.Context()

	// No engine: no-op.
	bare := memoryRuntime{}
	bare.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)

	// Only synthetic messages: no question, nothing retained.
	f := &fakeEngine{}
	m := memoryRuntime{}
	m.init(nil, f, nil, nil)
	m.RetainRun(ctx, "/ws", []messages.Message{
		messages.Human("<relevant-memory>\nx\n</relevant-memory>"),
		messages.Human("<queued-user-input>\ny\n</queued-user-input>"),
		messages.Human("<stop-hook>\nz\n</stop-hook>"),
		messages.Human("<task-fork>\nw\n</task-fork>"),
	}, nil, nil, nil)
	if len(f.events) != 0 {
		t.Fatalf("synthetic-only run retained: %v", f.events)
	}

	// A cancelled run records a cancelled outcome; errors are dropped silently.
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("real question")}, nil, context.Canceled, nil)
	if len(f.events) != 1 || !strings.HasSuffix(f.events[0], "cancelled") {
		t.Fatalf("events = %v", f.events)
	}

	// RecordEvent/Remember failures only warn.
	failing := &fakeEngine{eventErr: errors.New("e1"), rememberErr: errors.New("e2")}
	m2 := memoryRuntime{}
	m2.init(nil, failing, nil, nil)
	m2.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q")}, []messages.Message{messages.AI("answer")}, nil, nil)
	m2.RetainGoal(ctx, "/ws", "objective", string(GoalComplete), "summary", nil)
	m2.RecordTaskEvent(ctx, "/ws", "sub", "desc", "report", errors.New("task boom"))
	// A System 2 cycle with a failing Consolidate also only warns.
	m3 := memoryRuntime{}
	m3.init(nil, &consolidatingEngine{fakeEngine: &fakeEngine{}, err: errors.New("c1")},
		func() settings.Settings { return settings.Settings{LadyMSystem2: true} }, nil)
	m3.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q")}, nil, nil, nil)

	// RetainGoal outcome mapping and its subagent gates.
	g := &fakeEngine{}
	agent := &Agent{mem: memoryRuntime{}}
	agent.mem.init(nil, g, nil, nil)
	agent.RetainGoal(ctx, "obj", string(GoalBlocked), "", nil)
	if len(g.events) != 1 || !strings.Contains(g.events[0], "goal:blocked") {
		t.Fatalf("goal events = %v", g.events)
	}
	sub := &Agent{subagent: true}
	sub.RetainRun(ctx, []messages.Message{messages.Human("q")}, nil, nil, nil)
	sub.RetainGoal(ctx, "obj", "complete", "sum", nil)
	sub.recordTaskEvent(ctx, "n", "d", "r", nil)

	// runQuestion skips known synthetic wrappers.
	q := runQuestion([]messages.Message{
		messages.Human("<task-fork>\nnope\n</task-fork>"),
		messages.AI("mid"),
		messages.Human("actual request"),
	})
	if q != "actual request" {
		t.Fatalf("runQuestion = %q", q)
	}
	if got := runQuestion([]messages.Message{messages.AI("only")}); got != "" {
		t.Fatalf("runQuestion without humans = %q", got)
	}
}

// consolidatingEngine fails Consolidate on demand (System 2 error path).
type consolidatingEngine struct {
	*fakeEngine
	err error
}

func (c *consolidatingEngine) Consolidate(ctx context.Context, ws string, since float64) (string, error) {
	return "", c.err
}

// --- tasksummary.go ---

func TestConversationTailAndSummarizeTail(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("old"),
		messages.Human("kept user"),
		messages.AI("kept ai"),
		messages.Tool("t", strings.Repeat("z", 400)),
	}
	got := conversationTail(msgs, 3)
	if strings.Contains(got, "old") {
		t.Fatalf("tail not bounded: %q", got)
	}
	for _, want := range []string{"kept user", "kept ai", "tool:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tail missing %q: %q", want, got)
		}
	}
	if !strings.HasSuffix(got, "…\n") {
		t.Fatalf("long tool content not truncated: %q", got[len(got)-10:])
	}
	if got := summarizeTail("short", 10); got != "short" {
		t.Fatalf("summarizeTail = %q", got)
	}
}

func TestMaybeBeginTaskSummaryGenerates(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("Summarizing the work"))))
	a.summarizer = language.NewFakeChatModel(language.WithResponses(messages.AI("Writing the report")))
	if got := a.TaskSummary(); got != "" {
		t.Fatalf("initial summary = %q", got)
	}
	msgs := []messages.Message{messages.Human("do the thing")}
	// Subagents never generate summaries.
	a.subagent = true
	for range 8 {
		a.maybeBeginTaskSummary(t.Context(), msgs)
	}
	if a.TaskSummary() != "" {
		t.Fatal("subagent must not generate task summaries")
	}
	a.subagent = false
	a.taskState.mu.Lock()
	a.taskState.rounds = taskSummaryInterval - 1
	a.taskState.mu.Unlock()
	a.maybeBeginTaskSummary(t.Context(), msgs)
	waitFor(t, 5*time.Second, func() bool { return a.TaskSummary() == "Writing the report" }, "task summary generated")
}

// --- stream.go ---

func TestStreamDeltaHelpers(t *testing.T) {
	chunk := messages.Message{ContentBlocks: []messages.ContentBlock{
		messages.ReasoningBlock{Reasoning: "thinking hard"},
		messages.TextBlock{Text: "answer"},
		messages.PlainTextBlock{Text: "plain"},
	}}
	if got := ReasoningDeltaText(chunk); got != "thinking hard" {
		t.Fatalf("ReasoningDeltaText = %q", got)
	}
	if got := TextDelta(messages.Message{}); got != "" {
		t.Fatalf("empty TextDelta = %q", got)
	}
	if got := TextDelta(chunk); got != "answerplain" {
		t.Fatalf("TextDelta blocks = %q", got)
	}

	// MergeStreamChunks: first chunk seeds, later ones append/merge.
	var full messages.Message
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ID: "m1"})
	second := messages.Message{Content: " more", ToolCalls: []messages.ToolCall{{ID: "t1"}}}
	MergeStreamChunks(&full, second)
	if full.ID != "m1" || full.Content != " more" || len(full.ToolCalls) != 1 {
		t.Fatalf("full = %+v", full)
	}
	// Duplicate tool-call ID merges (name fills in, args merge).
	third := messages.Message{ToolCalls: []messages.ToolCall{{ID: "t1", Name: "Read", Args: map[string]any{"file_path": "x"}}}}
	MergeStreamChunks(&full, third)
	if len(full.ToolCalls) != 1 || full.ToolCalls[0].Name != "Read" || full.ToolCalls[0].Args["file_path"] != "x" {
		t.Fatalf("merged calls = %+v", full.ToolCalls)
	}
	// Usage metadata and response metadata replace when present.
	fourth := messages.Message{
		UsageMetadata:    messages.UsageMetadata{TotalTokens: 9},
		ResponseMetadata: map[string]any{"stop_reason": "end"},
	}
	MergeStreamChunks(&full, fourth)
	if full.UsageMetadata.TotalTokens != 9 || full.ResponseMetadata["stop_reason"] != "end" {
		t.Fatalf("metadata merge = %+v", full)
	}
	// A tool call without ID appends.
	MergeStreamChunks(&full, messages.Message{ToolCalls: []messages.ToolCall{{Name: "NoID"}}})
	if len(full.ToolCalls) != 2 {
		t.Fatalf("appended calls = %+v", full.ToolCalls)
	}
}

// --- reactivecompact.go ---

func TestIsPromptTooLongPatterns(t *testing.T) {
	if IsPromptTooLongError(nil) {
		t.Fatal("nil error is not prompt-too-long")
	}
	for _, msg := range []string{
		"prompt is too long: 10 > 5",
		"PROMPT TOO LONG",
		"maximum context length is 8192 tokens",
		"code: context_length_exceeded",
		"request exceeds the maximum number of tokens",
		"input length and max_tokens exceed context",
	} {
		if !IsPromptTooLongError(errors.New(msg)) {
			t.Errorf("IsPromptTooLongError(%q) = false", msg)
		}
	}
	if IsPromptTooLongError(errors.New("plain failure")) {
		t.Error("unrelated error misclassified")
	}
}

func TestReactiveCompactHistoryGuards(t *testing.T) {
	history := []messages.Message{messages.Human("hello")}
	// No summarizer: unchanged.
	bare := &Agent{}
	if got := bare.ReactiveCompactHistory(t.Context(), history); len(got) != 1 {
		t.Fatalf("nil summarizer changed history")
	}
	// Empty history: unchanged.
	a := &Agent{summarizer: language.NewFakeChatModel(language.WithResponses(messages.AI("s")))}
	if got := a.ReactiveCompactHistory(t.Context(), nil); got != nil {
		t.Fatalf("empty history = %v", got)
	}
	// PreCompact hook blocked: unchanged.
	blocker := hooks.NewRunner(hooks.Config{PreCompact: []hooks.Entry{{Hooks: []hooks.Hook{{Command: "b"}}}}},
		".", "s", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			return hooks.CommandResult{ExitCode: 2}, nil
		}))
	blocked := &Agent{summarizer: language.NewFakeChatModel(), hooks: blocker}
	if got := blocked.ReactiveCompactHistory(t.Context(), history); len(got) != 1 {
		t.Fatalf("blocked compaction changed history")
	}
	// Failing summarizer: history returned as-is (caller surfaces the error).
	failing := &Agent{summarizer: failingSummarizer{language.NewFakeChatModel()}}
	if got := failing.ReactiveCompactHistory(t.Context(), history); len(got) != 1 {
		t.Fatalf("failed compaction changed history")
	}
}

// --- recovery.go ---

func TestBeginOutputEscalationVariants(t *testing.T) {
	// No escalator configured.
	bare := &Agent{}
	if _, ok := bare.BeginOutputEscalation(); ok {
		t.Fatal("no escalator must report ok=false")
	}
	// Escalator error and nil model both fail closed.
	errEsc := &Agent{escalate: func() (language.ChatModel, error) { return nil, errors.New("no cap") }}
	if _, ok := errEsc.BeginOutputEscalation(); ok {
		t.Fatal("escalator error must report ok=false")
	}
	nilEsc := &Agent{escalate: func() (language.ChatModel, error) { return nil, nil }}
	if _, ok := nilEsc.BeginOutputEscalation(); ok {
		t.Fatal("nil escalated model must report ok=false")
	}
	// A working escalator swaps the model and the restore puts it back.
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("first"))))
	prev := a.CurrentModel()
	a.escalate = func() (language.ChatModel, error) {
		return language.NewFakeChatModel(language.WithResponses(messages.AI("escalated"))), nil
	}
	restore, ok := a.BeginOutputEscalation()
	if !ok {
		t.Fatal("escalation failed")
	}
	if a.CurrentModel() == prev {
		t.Fatal("model not swapped")
	}
	restore()
	if a.CurrentModel() != prev {
		t.Fatal("model not restored")
	}
}

func TestRecoverOutputLimitTurnErrors(t *testing.T) {
	history := []messages.Message{messages.Human("q")}
	truncated := []messages.Message{
		messages.System("s"),
		{Role: messages.RoleAI, Content: "cut off", ResponseMetadata: map[string]any{"stop_reason": "max_tokens"}},
	}
	turnErr := func(context.Context, []messages.Message) ([]messages.Message, error) {
		return nil, errors.New("turn failed")
	}
	// Escalated retry failure surfaces the error.
	esc := &Agent{escalate: func() (language.ChatModel, error) {
		return language.NewFakeChatModel(), nil
	}}
	if _, err := esc.RecoverOutputLimit(t.Context(), history, truncated, turnErr); err == nil {
		t.Fatal("escalated turn error must propagate")
	}
	// Without an escalator the continuation retry failure surfaces too.
	plain := &Agent{}
	if _, err := plain.RecoverOutputLimit(t.Context(), history, truncated, turnErr); err == nil {
		t.Fatal("continuation turn error must propagate")
	}
	// A non-truncated tail returns out unchanged.
	ok := []messages.Message{messages.AI("fine")}
	if got, err := plain.RecoverOutputLimit(t.Context(), history, ok, turnErr); err != nil || len(got) != 1 {
		t.Fatalf("non-truncated = %v, %v", got, err)
	}
}

// --- mcpauth.go ---

func TestMcpAuthToolErrorPath(t *testing.T) {
	a, err := New(Config{
		Model:        language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "t",
		McpAuth: func(ctx context.Context, server string) error {
			if server == "bad" {
				return errors.New("oauth failed")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := a.execute(t.Context(), "McpAuth", map[string]any{"server": "bad"}); !strings.Contains(res.Content, "oauth failed") {
		t.Fatalf("failed flow = %q", res.Content)
	}
	if res := a.execute(t.Context(), "McpAuth", map[string]any{"server": "good"}); !strings.Contains(res.Content, "Authorized MCP server good") {
		t.Fatalf("success = %q", res.Content)
	}
}

// --- refreshtools.go / toolbudget.go / queue.go / truncate.go ---

func TestSameToolSets(t *testing.T) {
	a, b := mustFromFunc("A"), mustFromFunc("B")
	if sameToolSets([]coretools.Tool{a}, []coretools.Tool{a, b}) {
		t.Fatal("different lengths must differ")
	}
	if sameToolSets([]coretools.Tool{a, b}, []coretools.Tool{b, b}) {
		t.Fatal("multiset mismatch must differ")
	}
	if !sameToolSets([]coretools.Tool{a, b}, []coretools.Tool{b, a}) {
		t.Fatal("order-insensitive compare failed")
	}
}

func TestTruncateAndRunesEdge(t *testing.T) {
	if got := truncate("anything", 0); got != "" {
		t.Fatalf("truncate(0) = %q", got)
	}
	if got := truncateRunes("abc", -1); got != "" {
		t.Fatalf("truncateRunes(-1) = %q", got)
	}
}

func TestQueueTaggedEmpty(t *testing.T) {
	a := &Agent{}
	a.QueueNotification("tag", "   ")
	if a.QueuedInputCount() != 0 {
		t.Fatal("blank notification must be dropped")
	}
	a.QueueInput("  ")
	if a.QueuedInputCount() != 0 {
		t.Fatal("blank input must be dropped")
	}
}

// --- toolsummary.go ---

func TestToolSummaryBufferBounds(t *testing.T) {
	var b toolSummaryBuffer
	// More than the cap: the oldest are evicted.
	for i := range maxPendingToolSummaries + 5 {
		b.add(&toolSummary{ids: []string{string(rune('a' + i%26))}, done: make(chan struct{})})
	}
	if got := b.settled(); len(got) != 0 {
		t.Fatalf("none settled yet: %v", got)
	}
	b.mu.Lock()
	size := len(b.list)
	b.mu.Unlock()
	if size != maxPendingToolSummaries {
		t.Fatalf("buffer = %d, want capped at %d", size, maxPendingToolSummaries)
	}
	// A settled summary with empty text is dropped, not returned.
	empty := &toolSummary{ids: []string{"e"}, done: make(chan struct{})}
	b.add(empty)
	close(empty.done)
	if got := b.settled(); len(got) != 0 {
		t.Fatalf("empty text leaked: %v", got)
	}
	// waitAll with a cancelled ctx returns what settled.
	pending := &toolSummary{ids: []string{"p"}, done: make(chan struct{})}
	b.add(pending)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := b.waitAll(ctx); got != nil {
		t.Fatalf("waitAll on cancelled ctx = %v", got)
	}
	close(pending.done)
}

func TestBeginToolUseSummaryJoinsBatches(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("summarizing")))) // unused path
	a.emitSummaries = true
	a.summarizer = language.NewFakeChatModel(language.WithResponses(messages.AI("Ran the tests")))
	calls := []messages.ToolCall{
		{ID: "1", Name: "Bash", Args: map[string]any{"command": "go test"}},
		{ID: "2", Name: "Read", Args: map[string]any{"file_path": "x"}},
	}
	results := []messages.Message{messages.Tool("1", "ok"), messages.Tool("2", "data")}
	a.beginToolUseSummary(t.Context(), calls, results, "checking things")
	if a.PendingToolUseSummaryCount() != 1 {
		t.Fatalf("pending = %d", a.PendingToolUseSummaryCount())
	}
	waitFor(t, 5*time.Second, func() bool {
		return len(a.TakeReadyToolUseSummaries()) > 0 || a.PendingToolUseSummaryCount() == 0
	}, "summary settled")
	// Subagents and disabled agents skip generation entirely.
	b := &Agent{subagent: true, emitSummaries: true, summarizer: language.NewFakeChatModel()}
	b.beginToolUseSummary(t.Context(), calls, results, "")
	if b.PendingToolUseSummaryCount() != 0 {
		t.Fatal("subagent must not buffer summaries")
	}
}

// --- streamingexec.go ---

func TestSpeculativeRegistryBounds(t *testing.T) {
	var r speculativeRegistry
	f := newToolFuture(nil)
	if !r.add("id", f) {
		t.Fatal("first add must register")
	}
	if r.add("id", newToolFuture(nil)) {
		t.Fatal("duplicate add must be rejected")
	}
	// Eviction past the cap keeps the registry bounded.
	for i := range maxSpeculativeFutures + 10 {
		r.add(string(rune(i%128))+string(rune(i/128))+"-"+string(rune(i%10)), newToolFuture(nil))
	}
	r.mu.Lock()
	size := len(r.futures)
	r.mu.Unlock()
	if size > maxSpeculativeFutures {
		t.Fatalf("registry = %d, want <= %d", size, maxSpeculativeFutures)
	}
	// cancel aborts and removes.
	ctx, cancel := context.WithCancel(t.Context())
	g := newToolFuture(cancel)
	r.add("gate", g)
	r.cancel("gate")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel did not abort the future")
	}
	if r.get("gate") != nil {
		t.Fatal("cancelled future not removed")
	}
}

func TestDispatchSpeculativeDuplicateIgnored(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true})))
	call := messages.ToolCall{ID: "d1", Name: "Read", Args: map[string]any{"file_path": "x.go"}}
	if !a.dispatchSpeculative(t.Context(), call) {
		t.Fatal("first dispatch must start")
	}
	if a.dispatchSpeculative(t.Context(), call) {
		t.Fatal("duplicate dispatch must be ignored")
	}
	// Wait for the future to finish so the test does not race the registry.
	f := a.spec.get("d1")
	if f != nil {
		_ = f.wait()
	}
	a.spec.consume("d1")
	// Calls the gate would interrupt are never dispatched.
	if a.dispatchSpeculative(t.Context(), messages.ToolCall{ID: "d2", Name: "AskUserQuestion"}) {
		t.Fatal("interaction gate must not pre-run")
	}
	if a.dispatchSpeculative(t.Context(), messages.ToolCall{ID: "", Name: "Read"}) {
		t.Fatal("id-less call must not dispatch")
	}
}

func TestRunToolCallHookFeedback(t *testing.T) {
	runner := hooks.NewRunner(hooks.Config{
		PreToolUse:  []hooks.Entry{{Matcher: "Read", Hooks: []hooks.Hook{{Command: "pre"}}}},
		PostToolUse: []hooks.Entry{{Matcher: "Read", Hooks: []hooks.Hook{{Command: "post"}}}},
	}, ".", "s", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		if command == "pre" {
			return hooks.CommandResult{Stdout: "pre-context"}, nil
		}
		return hooks.CommandResult{Stdout: "post-context"}, nil
	}))
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Hooks:          runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := a.runToolCall(t.Context(), messages.ToolCall{ID: "h1", Name: "Read", Args: map[string]any{"file_path": "missing.txt"}})
	if !strings.Contains(msg.Content, "pre-context") || !strings.Contains(msg.Content, "post-context") {
		t.Fatalf("hook feedback missing from result: %q", msg.Content)
	}

	// A denying PreToolUse hook short-circuits execution.
	deny := hooks.NewRunner(hooks.Config{PreToolUse: []hooks.Entry{{Matcher: "Read", Hooks: []hooks.Hook{{Command: "deny"}}}}},
		".", "s", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			return hooks.CommandResult{ExitCode: 2, Stdout: "no reads"}, nil
		}))
	b, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Hooks:          deny,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg = b.runToolCall(t.Context(), messages.ToolCall{ID: "h2", Name: "Read", Args: map[string]any{"file_path": "x"}})
	if !strings.Contains(msg.Content, "no reads") {
		t.Fatalf("deny result = %q", msg.Content)
	}
}

func TestToolMessageFromResultImage(t *testing.T) {
	m := toolMessageFromResult("i1", "text", &coretools.Result{
		Metadata: map[string]any{
			tools.MetaImageBase64: "abc123",
			tools.MetaImageMime:   "image/png",
		},
	})
	if len(m.ContentBlocks) != 1 {
		t.Fatalf("blocks = %d", len(m.ContentBlocks))
	}
	if img, ok := m.ContentBlocks[0].(messages.ImageBlock); !ok || img.Base64 != "abc123" || img.MimeType != "image/png" {
		t.Fatalf("image block = %+v", m.ContentBlocks[0])
	}
	// No metadata / empty b64: plain text message.
	if m := toolMessageFromResult("i2", "text", &coretools.Result{}); len(m.ContentBlocks) != 0 {
		t.Fatalf("blocks = %d", len(m.ContentBlocks))
	}
	if m := toolMessageFromResult("i3", "text", nil); m.Content != "text" {
		t.Fatalf("nil result = %q", m.Content)
	}
	if m := toolMessageFromResult("i4", "text", &coretools.Result{Metadata: map[string]any{tools.MetaImageBase64: ""}}); len(m.ContentBlocks) != 0 {
		t.Fatalf("empty b64 blocks = %d", len(m.ContentBlocks))
	}
}

// --- tasklist.go ---

func TestTaskListUpdateAndClaimEdges(t *testing.T) {
	l := newTaskList()
	a := l.CreateDeps("a", "", "", nil)
	b := l.CreateDeps("b", "", "", []string{a.ID})
	ghost := l.CreateDeps("ghost-dep", "", "", []string{"no-such-task"})
	// Claim with an unsatisfiable dependency fails.
	if _, ok := l.Claim(ghost.ID, "w"); ok {
		t.Fatal("claim with a missing dependency must fail")
	}
	if _, ok := l.Claim(b.ID, "w"); ok {
		t.Fatal("claim before the dependency completes must fail")
	}
	if _, ok := l.Claim("nope", "w"); ok {
		t.Fatal("claim on unknown id must fail")
	}
	// Update on unknown id fails; partial updates keep zero fields.
	if _, ok := l.Update("nope", "", "", ""); ok {
		t.Fatal("update on unknown id must fail")
	}
	rec, ok := l.Update(a.ID, "", "new subject", "")
	if !ok || rec.Subject != "new subject" || rec.Status != TaskRecordPending {
		t.Fatalf("partial update = %+v", rec)
	}
	// Complete the dependency and claim b.
	l.Update(a.ID, TaskRecordCompleted, "", "")
	if _, ok := l.Claim(b.ID, "w"); !ok {
		t.Fatal("claim after dependencies complete failed")
	}
	// ReleaseForOwner re-checks owner and status.
	if l.ReleaseForOwner(b.ID, "other") {
		t.Fatal("foreign release must fail")
	}
	l.Update(b.ID, TaskRecordCompleted, "", "")
	if l.ReleaseForOwner(b.ID, "w") {
		t.Fatal("release of a completed task must fail")
	}
	// Restore recomputes the id counter.
	l.Restore([]TaskRecord{{ID: "task-9", Subject: "x"}})
	if _, ok := l.Get("task-9"); !ok {
		t.Fatal("restore lost the task")
	}
	if next := l.Create("n", "", ""); next.ID != "task-10" {
		t.Fatalf("next id = %s", next.ID)
	}
}

func TestRetainRunRecordsFileOperation(t *testing.T) {
	ctx := t.Context()

	f := &fakeEngine{}
	m := memoryRuntime{}
	m.init(nil, f, nil, nil)

	// A mixed create+update run maps to the canonical "update" action and
	// carries the deduplicated touched paths as resources.
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, []tools.FileChange{
		{Path: "a.go", Before: []byte("x"), After: []byte("y")},
		{Path: "b.go", Before: nil, After: []byte("new")},
	})
	if len(f.ops) != 1 {
		t.Fatalf("operations recorded = %d, want 1", len(f.ops))
	}
	op := f.ops[0]
	if op.Action != "update" {
		t.Fatalf("op action = %q, want update", op.Action)
	}
	if len(op.Resources) != 2 || op.Resources[0] != "a.go" || op.Resources[1] != "b.go" {
		t.Fatalf("op resources = %v, want [a.go b.go]", op.Resources)
	}
	if op.Outcome != "success" {
		t.Fatalf("op outcome = %q, want success", op.Outcome)
	}

	// A run with no file changes records no operation.
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q2")}, nil, nil, nil)
	if len(f.ops) != 1 {
		t.Fatalf("changeless run recorded an operation: %v", f.ops)
	}

	// A create-only run maps to "create".
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q3")}, nil, nil, []tools.FileChange{
		{Path: "new.go", Before: nil, After: []byte("n")},
	})
	if f.ops[1].Action != "create" {
		t.Fatalf("create-only action = %q, want create", f.ops[1].Action)
	}

	// A cancelled run keeps the operation outcome honest.
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q4")}, nil, context.Canceled, []tools.FileChange{
		{Path: "a.go", Before: []byte("x"), After: []byte("z")},
	})
	if f.ops[2].Outcome != "cancelled" {
		t.Fatalf("cancelled outcome = %q, want cancelled", f.ops[2].Outcome)
	}

	// RecordOperation failures only warn (best-effort, like RecordEvent).
	f.opErr = errors.New("op boom")
	m.RetainRun(ctx, "/ws", []messages.Message{messages.Human("q5")}, nil, nil, []tools.FileChange{
		{Path: "a.go", Before: []byte("x"), After: []byte("w")},
	})
}
