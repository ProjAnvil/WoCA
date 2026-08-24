package agent

// gaps2_test.go targets the remaining uncovered branches across the package:
// constructor fallbacks, interrupt-envelope edge cases, memory prefetch
// paths, team spawn races, goal-graph resume/pause paths and tool-panic
// recovery in the speculative executor. Deterministic throughout: every
// asynchronous step is observed through a channel, a recorded call count or
// the shared waitFor helper — never a fixed sleep.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/bus"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

// --- small pure-function gaps (wscache / memoryscope / cron / interruptenc) ---

func TestWSCacheSetReplacesExistingKey(t *testing.T) {
	c := newWSCache[string](2)
	c.Set("a", "1")
	c.Set("a", "2")
	if v, ok := c.Get("a"); !ok || v != "2" {
		t.Fatalf("Get(a) = %q, %v; want 2, true", v, ok)
	}
	if c.ll.Len() != 1 {
		t.Fatalf("cache grew on update: len = %d", c.ll.Len())
	}
}

func TestMemoryScopeFromNilContext(t *testing.T) {
	if s := memoryScopeFrom(nil); s != nil {
		t.Fatalf("memoryScopeFrom(nil) = %v, want nil", s)
	}
	if s := memoryScopeFrom(context.Background()); s != nil {
		t.Fatalf("memoryScopeFrom(plain ctx) = %v, want nil", s)
	}
}

func TestParseCronInvalidFieldValue(t *testing.T) {
	if _, err := parseCron("* * * * notanumber"); err == nil {
		t.Fatal("expected invalid-value error for non-numeric dow field")
	} else if !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("error = %v, want invalid value", err)
	}
}

func TestCronListSortsByID(t *testing.T) {
	s := newCronScheduler(func(tag, text string) {})
	if _, err := s.Add("b", "p2", "0 * * * *"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("a", "p1", "5 * * * *"); err != nil {
		t.Fatal(err)
	}
	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ID != "cron-1" || entries[1].ID != "cron-2" {
		t.Fatalf("entries not id-ordered: %+v", entries)
	}
}

func TestInterruptEnvelopeEdgeCases(t *testing.T) {
	// Pass-through of JSON-native (non-struct) payloads.
	if got := encodeInterruptPayload(42); got != 42 {
		t.Fatalf("encode(42) = %v", got)
	}
	if got := encodeInterruptPayload("plain"); got != "plain" {
		t.Fatalf("encode(string) = %v", got)
	}
	// Marshal failure degrades to pass-through (NaN cannot be JSON-encoded).
	bad := PermissionRequest{ToolArgs: map[string]any{"nan": math.NaN()}}
	if got := encodeInterruptPayload(bad); !reflect.DeepEqual(got, any(bad)) {
		t.Fatalf("encode(bad) = %T, want the original value", got)
	}
	// Envelope decode: unknown kind passes the map through untouched.
	unk := map[string]any{wocaInterruptTag: "agent.Nope"}
	if got := decodeInterruptPayload(unk); !reflect.DeepEqual(got, any(unk)) {
		t.Fatalf("decode(unknown) = %v", got)
	}
	// Envelope decode: a map that cannot be re-marshaled passes through.
	nan := map[string]any{wocaInterruptTag: "agent.PermissionRequest", "nan": math.NaN()}
	if got := decodeInterruptPayload(nan); !reflect.DeepEqual(got, any(nan)) {
		t.Fatalf("decode(nan map) = %v", got)
	}
	// Envelope decode: right kind, wrong field types passes through.
	typed := map[string]any{wocaInterruptTag: "agent.PermissionRequest", "tool_args": 123}
	if got := decodeInterruptPayload(typed); !reflect.DeepEqual(got, any(typed)) {
		t.Fatalf("decode(mistyped) = %v", got)
	}
	// AskUserAnswer round-trip through the envelope.
	ans := map[string]any{wocaInterruptTag: "agent.AskUserAnswer", "Answers": []any{"a", "b"}}
	got := decodeInterruptPayload(ans)
	au, ok := got.(AskUserAnswer)
	if !ok || len(au.Answers) != 2 || au.Answers[0] != "a" {
		t.Fatalf("decode(AskUserAnswer) = %#v", got)
	}
	// GoalDelta round-trip (derefAny's *GoalDelta arm).
	gd := map[string]any{wocaInterruptTag: "agent.GoalDelta", "Message": map[string]any{"role": "ai", "content": "hi"}}
	got = decodeInterruptPayload(gd)
	delta, ok := got.(GoalDelta)
	if !ok || delta.Message.Content != "hi" {
		t.Fatalf("decode(GoalDelta) = %#v", got)
	}

	// Resume-value envelope edges.
	if got := decodeResumeValue(wocaResumePrefix + "nokolseparator"); !reflect.DeepEqual(got, any(wocaResumePrefix+"nokolseparator")) {
		t.Fatalf("decodeResume(no colon) = %v", got)
	}
	if got := decodeResumeValue(wocaResumePrefix + "agent.Unknown:{}"); !reflect.DeepEqual(got, any(wocaResumePrefix+"agent.Unknown:{}")) {
		t.Fatalf("decodeResume(unknown kind) = %v", got)
	}
	blob := wocaResumePrefix + "agent.AskUserAnswer:not-json"
	if got := decodeResumeValue(blob); !reflect.DeepEqual(got, any(blob)) {
		t.Fatalf("decodeResume(bad json) = %v", got)
	}
	okAns := decodeResumeValue(encodeResumeValue(AskUserAnswer{Answers: []string{"yes"}}))
	au2, ok := okAns.(AskUserAnswer)
	if !ok || len(au2.Answers) != 1 {
		t.Fatalf("resume round-trip = %#v", okAns)
	}
}

// --- memorystore.go: kv-list failure, hit caps, per-run scopes, term cap ---

// failConnector is a driver.Connector whose every Connect fails; Connect
// signals the attempt on a channel so tests can wait deterministically.
type failConnector struct {
	once   sync.Once
	called chan struct{}
}

func (c *failConnector) Connect(context.Context) (driver.Conn, error) {
	c.once.Do(func() { close(c.called) })
	return nil, errors.New("db down")
}

func (c *failConnector) Driver() driver.Driver { return nil }

func TestMemoryBeginKVListErrorSkipsAttachments(t *testing.T) {
	fc := &failConnector{called: make(chan struct{})}
	kv := store.NewSQL(sql.OpenDB(fc), "sqlite", "broken")
	m := memoryRuntime{store: kv}
	m.Begin(context.Background(), []messages.Message{messages.Human("alpha beta")}, "/ws")
	select {
	case <-fc.called:
	case <-time.After(2 * time.Second):
		t.Fatal("store.List never attempted the connection")
	}
	if got := m.TakeAttachments(context.Background()); len(got) != 0 {
		t.Fatalf("failed list must produce no attachments: %v", got)
	}
}

func TestMemoryBeginKVCapsHitsAndUsesScope(t *testing.T) {
	kv := store.NewMemory()
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if err := kv.Put(ctx, "ns", fmt.Sprintf("k%d", i), fmt.Sprintf("alpha note %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	m := memoryRuntime{store: kv}
	scopeCtx := WithMemoryScope(ctx)
	m.Begin(scopeCtx, []messages.Message{messages.Human("alpha")}, "/ws")
	sc := memoryScopeFrom(scopeCtx)
	waitFor(t, 2*time.Second, func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return sc.entries != nil
	}, "scoped kv prefetch to settle")
	sc.mu.Lock()
	got := len(sc.entries)
	sc.mu.Unlock()
	if got != maxMemoryAttachments*2 {
		t.Fatalf("scoped hits = %d, want the %d cap", got, maxMemoryAttachments*2)
	}
	atts := m.TakeAttachments(scopeCtx)
	if len(atts) != maxMemoryAttachments {
		t.Fatalf("attachments = %d, want %d", len(atts), maxMemoryAttachments)
	}
}

func TestMemoryBeginEngineUsesScope(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{
		ID: "h1", Layer: "L2", Type: "fact", Summary: "scoped hit", Content: "scoped content", Score: 0.5,
	}}}
	m := memoryRuntime{engine: f}
	scopeCtx := WithMemoryScope(context.Background())
	m.Begin(scopeCtx, []messages.Message{messages.Human("query words")}, "/ws")
	waitFor(t, 2*time.Second, func() bool { return len(f.recordedRecalls()) > 0 }, "scoped engine recall")
	atts := m.TakeAttachments(scopeCtx)
	if len(atts) != 1 || !strings.Contains(atts[0].Content, "scoped hit") {
		t.Fatalf("scoped engine attachments = %+v", atts)
	}
}

func TestBuildAttachmentsSkipsEmptyAndCapsEntries(t *testing.T) {
	m := memoryRuntime{}
	// A hit whose summary and content are both empty is marked injected but
	// produces no attachment.
	out := m.buildAttachments(nil, []memory.Hit{{ID: "h-empty", Layer: "L", Type: "fact"}})
	if len(out) != 0 {
		t.Fatalf("empty hit produced attachments: %+v", out)
	}
	// The entries loop stops at the attachment cap.
	entries := make([]store.Entry, 0, 7)
	for i := 0; i < 7; i++ {
		entries = append(entries, store.Entry{
			Namespace: "ns", Key: fmt.Sprintf("k%d", i), Value: []byte(fmt.Sprintf(`"value %d"`, i)),
		})
	}
	out = m.buildAttachments(entries, nil)
	if len(out) != maxMemoryAttachments {
		t.Fatalf("entry attachments = %d, want %d", len(out), maxMemoryAttachments)
	}
}

func TestMemoryTermsCapAtTwelve(t *testing.T) {
	msg := messages.Human("aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk llll mmmm nnnn")
	terms := memoryTerms([]messages.Message{msg})
	if len(terms) != 12 {
		t.Fatalf("terms = %d (%v), want the 12 cap", len(terms), terms)
	}
}

// --- memorytools / refreshtools / recovery / reactivecompact / tasksummary ---

func TestSearchCodeToolReportsHits(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	a.mem.SetEngine(&fakeEngine{})
	for _, tl := range a.memoryTools() {
		a.toolByName[tl.Name()] = tl
	}
	tool, ok := a.toolByName["search_code"]
	if !ok {
		t.Fatal("search_code tool not present with an engine wired")
	}
	res, err := tool.Invoke(context.Background(), map[string]any{"query": "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "(no hits)" {
		t.Fatalf("search_code content = %q", res.Content)
	}
}

func TestRefreshExtraToolsWithoutHook(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if a.refreshExtraTools(context.Background()) {
		t.Fatal("refresh without a hook must report no change")
	}
}

func TestBeginOutputEscalationFailures(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	a.escalate = func() (language.ChatModel, error) { return nil, errors.New("escalator boom") }
	if _, ok := a.BeginOutputEscalation(); ok {
		t.Fatal("escalator error must report ok=false")
	}
	a.escalate = func() (language.ChatModel, error) { return nil, nil }
	if _, ok := a.BeginOutputEscalation(); ok {
		t.Fatal("nil escalator model must report ok=false")
	}
	// A model that cannot bind the tool set fails the swap.
	a.escalate = func() (language.ChatModel, error) {
		return &bindFailModel{language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}))}, nil
	}
	if _, ok := a.BeginOutputEscalation(); ok {
		t.Fatal("unbindable escalator model must report ok=false")
	}
}

func TestReactiveCompactSummarizerErrorReturnsHistory(t *testing.T) {
	a := newCompactAgent(t, failingSummarizer{language.NewFakeChatModel()})
	hist := make([]messages.Message, 5)
	for i := range hist {
		hist[i] = messages.Human(strings.Repeat(fmt.Sprintf("question %d ", i), 40))
	}
	out := a.ReactiveCompactHistory(context.Background(), hist)
	if len(out) == 0 {
		t.Fatal("failed summarization must return the (snipped) history, not nothing")
	}
}

func TestMaybeBeginTaskSummaryInvokeErrorBestEffort(t *testing.T) {
	a := &Agent{summarizer: failingSummarizer{language.NewFakeChatModel()}}
	msgs := []messages.Message{messages.Human("do a thing")}
	for i := 0; i < taskSummaryInterval; i++ {
		a.maybeBeginTaskSummary(context.Background(), msgs)
	}
	waitFor(t, 2*time.Second, func() bool {
		a.taskState.mu.Lock()
		defer a.taskState.mu.Unlock()
		return !a.taskState.pending
	}, "failed summary to clear pending")
	if got := a.TaskSummary(); got != "" {
		t.Fatalf("failed summary must leave latest empty, got %q", got)
	}
}

// --- retain.go: System 2 consolidation error is warned, never fatal ---

// consolidateFailEngine fails Consolidate and signals the attempt.
type consolidateFailEngine struct {
	*fakeEngine
	once   sync.Once
	called chan struct{}
}

func (c *consolidateFailEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	c.once.Do(func() { close(c.called) })
	return "", errors.New("consolidate boom")
}

func TestRetainRunSystem2ConsolidateError(t *testing.T) {
	e := &consolidateFailEngine{fakeEngine: &fakeEngine{}, called: make(chan struct{})}
	m := memoryRuntime{engine: e, loadSettings: func() settings.Settings {
		s := settings.Default()
		s.LadyMAutoRetain = true
		s.LadyMSystem2 = true
		s.LadyMSystem2Interval = 60
		return s
	}}
	m.RetainRun(context.Background(), "/ws-s2fail",
		[]messages.Message{messages.Human("what is this about?")},
		[]messages.Message{messages.AI("an answer")},
		nil)
	select {
	case <-e.called:
	case <-time.After(2 * time.Second):
		t.Fatal("Consolidate never ran")
	}
}

// --- contextinfo.go: workspace override, memory share, extra tools ---

func TestContextBreakdownWorkspacesMemoryAndExtras(t *testing.T) {
	ws := t.TempDir()
	big := strings.Repeat("project memory content ", 200)
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ws)
	extra, err := coretools.FromFunc("ExtraEcho", "test extra tool", func(ctx context.Context, in struct{}) (coretools.Result, error) {
		return coretools.Result{Content: "extra"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       reg,
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		SystemPromptFor: func(workDir, model string) string {
			return "tiny"
		},
		ExtraTools: []coretools.Tool{extra},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runws.With(context.Background(), ws)
	info := a.ContextBreakdown(ctx, []messages.Message{
		messages.System("compact summary"),
		messages.Human("hello"),
	})
	var mem, extraCat, summary *ContextCategory
	for i := range info.Categories {
		c := &info.Categories[i]
		switch c.Key {
		case "memory":
			mem = c
		case "tools_extra":
			extraCat = c
		case "history_summary":
			summary = c
		}
	}
	if mem == nil || mem.Tokens <= 0 || mem.Count == 0 {
		t.Fatalf("memory category missing: %+v", info.Categories)
	}
	// The tiny prompt is smaller than the memory share: the base clamps to 0.
	if c := info.Categories[0]; c.Key != "system" || c.Tokens != 0 {
		t.Fatalf("system category = %+v, want clamped to 0", c)
	}
	if extraCat == nil || extraCat.Count != 1 {
		t.Fatalf("extra tools category missing: %+v", info.Categories)
	}
	if summary == nil || summary.Count != 1 {
		t.Fatalf("history summary category missing: %+v", info.Categories)
	}
}

// --- worktree.go: ExitWorktree remove failure surfaces the git error ---

func TestExitWorktreeRemoveFailure(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	ctx := context.Background()
	ghost := filepath.Join(t.TempDir(), "ghost-worktree")
	a.registry.Dir().Enter(ctx, ghost)
	tool, ok := a.toolByName["ExitWorktree"]
	if !ok {
		t.Fatal("ExitWorktree tool missing")
	}
	res, err := tool.Invoke(ctx, map[string]any{"remove": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "removal failed") {
		t.Fatalf("exit content = %q, want removal failure", res.Content)
	}
}

// --- team.go: spawn race, typed subagents, duplicate spawn, active count ---

// racingTeamFactory simulates the spawn race: while Spawn is between its two
// membership checks (calling the factory), another spawn registers the name.
type racingTeamFactory struct {
	tm    *TeamManager
	inner *Agent
}

func (f *racingTeamFactory) newSubagent(ctx context.Context, def *agents.Definition) (*Agent, error) {
	f.tm.mu.Lock()
	f.tm.members["dup"] = &teammate{name: "dup"}
	f.tm.mu.Unlock()
	return f.inner, nil
}

func TestTeamSpawnLostRace(t *testing.T) {
	inner := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	tm := &TeamManager{
		factory: nil,
		bus:     bus.New(),
		list:    newTaskList(),
		members: map[string]*teammate{},
		pokes:   map[string]chan struct{}{},
	}
	tm.factory = &racingTeamFactory{tm: tm, inner: inner}
	err := tm.Spawn(context.Background(), "dup", "prompt", nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Spawn race error = %v, want already exists", err)
	}
	if _, stuck := tm.members["dup"]; !stuck {
		t.Fatal("the racing member should stay registered")
	}
}

func TestTeamSpawnToolTypedSubagentAndDuplicate(t *testing.T) {
	mateModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("mate done")),
	)
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("lead idle"))),
		SubagentModel:  mateModel,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "lead",
		PermissionMode: config.ModeBypass,
		Agents: []agents.Definition{{
			Name:   "coder",
			Prompt: "You write code.",
			Tools:  []string{"Read", "Grep"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	spawn, ok := a.toolByName["TeamSpawn"]
	if !ok {
		t.Fatal("TeamSpawn tool missing")
	}
	res, err := spawn.Invoke(context.Background(), map[string]any{
		"name": "w1", "prompt": "help out", "subagent_type": "coder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Spawned teammate w1") {
		t.Fatalf("spawn result = %q", res.Content)
	}
	if a.ActiveTeammateCount() != 1 {
		t.Fatalf("active teammates = %d, want 1", a.ActiveTeammateCount())
	}
	// Duplicate name surfaces the spawn error through the tool.
	if _, err := spawn.Invoke(context.Background(), map[string]any{
		"name": "w1", "prompt": "again", "subagent_type": "coder",
	}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate spawn error = %v", err)
	}
	if team := a.currentTeam(); team != nil {
		team.Close()
	}
}

// --- task.go: background-task cap surfaces through runTask ---

func TestRunTaskBackgroundCapReached(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	for i := 0; i < maxBackgroundTasks; i++ {
		if _, err := a.tasks.start("s", "d"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.runTask(context.Background(), taskArgs{Description: "overflow", RunInBackground: true})
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("cap error = %v", err)
	}
}

// --- agent.go: constructor fallback, accessors, compaction early-out ---

func TestNewCheckpointDirFallsBackToMemorySaver(t *testing.T) {
	// A regular file where a directory is expected makes the sqlite saver
	// fail to open its database; construction must fall back, not fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Model:         language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:      tools.NewRegistry(t.TempDir()),
		SystemPrompt:  "t",
		CheckpointDir: blocker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.saver == nil {
		t.Fatal("fallback saver missing")
	}
}

func TestZeroAgentLSPStats(t *testing.T) {
	a := &Agent{}
	if active, broken := a.LSPStats(); active != 0 || broken != 0 {
		t.Fatalf("LSPStats on zero agent = %d, %d", active, broken)
	}
}

func TestCompactHistoryUnderLimitReturnsUnchanged(t *testing.T) {
	a := newCompactAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("summary"))))
	hist := []messages.Message{messages.Human("hi")}
	out := a.CompactHistory(context.Background(), hist)
	if len(out) != 1 || out[0].Content != "hi" {
		t.Fatalf("small history changed by compaction: %+v", out)
	}
}

// --- agent.go: observedNode error / interrupt / panic paths with obs wired ---

// streamErrModel fails every Stream call (Invoke keeps working for other
// callers); BindTools returns itself so the wrapper survives rebinding.
type streamErrModel struct {
	*language.FakeChatModel
}

func (m *streamErrModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }

func (m *streamErrModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return nil, errors.New("stream boom")
}

// streamPanicModel is deliberately NOT tested here: a node panic that is not
// a GraphInterrupt is re-panicked by the langgraph runtime inside one of its
// own task goroutines, which would kill the test process. The interrupt path
// below exercises the recover arms that are safely reachable.

func newObsAgent(t *testing.T, model language.ChatModel, mode config.PermissionMode) *Agent {
	t.Helper()
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: mode,
		Obs:            obslog.New(t.TempDir(), true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestObservedNodeRecordsModelError(t *testing.T) {
	a := newObsAgent(t, &streamErrModel{language.NewFakeChatModel()}, config.ModeBypass)
	if _, err := a.Invoke(context.Background(), []messages.Message{messages.Human("hi")}, "t"); err == nil {
		t.Fatal("stream error must fail the run")
	}
}

func TestObservedNodeRecordsInterrupt(t *testing.T) {
	a := newObsAgent(t, gatedWriteModel("done"), config.ModeDefault)
	_, err := a.Invoke(context.Background(), []messages.Message{messages.Human("write")}, "t")
	ie := interruptFromError(t, err)
	if _, ok := ie.Value.(PermissionRequest); !ok {
		t.Fatalf("interrupt value = %T", ie.Value)
	}
}

// --- agent.go: toolsNode gate/hook error propagation ---

func TestToolsNodeGateDecodeErrorFailsRun(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
				ID:   "q1",
				Name: "AskUserQuestion",
				Args: map[string]any{"questions": []any{math.NaN()}},
			}}},
			messages.AI("never reached"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("ask")}, "t")
	if err == nil {
		t.Fatal("undecodable AskUserQuestion args must fail the run")
	}
}

func TestToolsNodePreToolUseHookErrorFailsRun(t *testing.T) {
	hr := hooks.NewRunner(
		hooks.Config{PreToolUse: []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Command: "pre"}}}}},
		".", "s1",
		hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			return hooks.CommandResult{}, errors.New("hook exec boom")
		}),
	)
	a, err := New(Config{
		Model:          gatedWriteModel("never"),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"Write"}},
		Hooks:          hr,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("write")}, "t")
	if err == nil || !strings.Contains(err.Error(), "hook exec boom") {
		t.Fatalf("hook error = %v, want hook exec boom", err)
	}
}

// --- streamingexec.go: speculative dispatch survives a panicking tool ---

func TestDispatchSpeculativeRecoversToolPanic(t *testing.T) {
	panicker, err := coretools.FromFunc("Read", "panics like a read", func(ctx context.Context, a struct{}) (coretools.Result, error) {
		panic("read boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(panicker) // replaces the real Read; Read is CatRead → speculatable
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       reg,
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := messages.ToolCall{ID: "sp1", Name: "Read", Args: map[string]any{}}
	if !a.dispatchSpeculative(context.Background(), call) {
		t.Fatal("expected the call to be dispatched speculatively")
	}
	f := a.spec.get("sp1")
	if f == nil {
		t.Fatal("future missing after dispatch")
	}
	msg := f.wait()
	if !strings.Contains(msg.Content, `tool "Read" panicked: read boom`) {
		t.Fatalf("recovered message = %q", msg.Content)
	}
	a.spec.consume("sp1")
}

// --- goalgraph.go: node guards, reply composition, token clamp ---

func TestGoalGraphNodeGuards(t *testing.T) {
	rt := lgruntime.NewRuntime(context.Background())
	// goalBeginCycle on a goal-less agent reports stop.
	bare := &Agent{}
	if bare.goalBeginCycle() {
		t.Fatal("goalBeginCycle without a goal must report false")
	}
	// The assess node ends the loop when the goal is no longer active.
	paused := &Agent{}
	paused.goal.goal = &Goal{Status: GoalPaused}
	out, err := paused.goalAssessNode()(rt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cmd, ok := out.(*lgtypes.Command); !ok || len(cmd.Goto) != 1 || cmd.Goto[0] != lgtypes.END {
		t.Fatalf("assess(paused) = %#v", out)
	}
	// The evaluate node ends the loop when the goal was cleared.
	cleared := &Agent{}
	out, err = cleared.goalEvaluateNode()(rt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cmd, ok := out.(*lgtypes.Command); !ok || len(cmd.Goto) != 1 || cmd.Goto[0] != lgtypes.END {
		t.Fatalf("evaluate(cleared) = %#v", out)
	}
	// The unified reply degrades to the bare report without executor text.
	if got := goalUnifiedReply(nil, "the report"); got != "the report" {
		t.Fatalf("unified reply = %q", got)
	}
	// Token usage clamps at zero when the base exceeds the session total.
	clamped := &Agent{}
	clamped.goal.goal = &Goal{MaxTokens: 100}
	clamped.goalRefreshTokens(5000)
	if clamped.goal.goal.TokensUsed != 0 {
		t.Fatalf("TokensUsed = %d, want 0", clamped.goal.goal.TokensUsed)
	}
}

// --- goalgraph.go: assess trims long note lists on the second cycle ---

func TestGoalAssessTrimsLongNotes(t *testing.T) {
	progress := func(id, note string) messages.Message {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: id, Name: "UpdateGoal",
			Args: map[string]any{"action": "progress", "note": note},
		}}}
	}
	a := newGoalAgent(t,
		messages.AI("cycle one plan"), // assess 1
		progress("n1", "first note"),  // execute 1
		progress("n2", "second note"),
		progress("n3", "third note"),
		progress("n4", "fourth note"),
		messages.AI("cycle one work done"),                           // execute 1 final
		messages.AI(`{"verdict":"continue","gaps":"more"}`),          // evaluate 1
		messages.AI("cycle two plan"),                                // assess 2 (notes > 3)
		messages.AI("cycle two work"),                                // execute 2
		messages.AI(`{"verdict":"complete","evidence":"all green"}`), // evaluate 2
	)
	if _, err := a.SetGoal("trim the notes", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	stream, err := a.GoalStream(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	res := pumpGoalChunks(t, stream)
	if res.err != nil {
		t.Fatalf("goal stream error: %v", res.err)
	}
	g := a.Goal()
	if g.Status != GoalComplete {
		t.Fatalf("status = %q, want complete", g.Status)
	}
	if len(g.Notes) != 4 {
		t.Fatalf("notes = %d, want 4 (%v)", len(g.Notes), g.Notes)
	}
}

// --- goalgraph.go: GoalResume re-pauses and early-consumer break ---

func TestGoalResumeRepausesAndBreaks(t *testing.T) {
	writeCall := func(id, path string) messages.Message {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: id, Name: "Write", Args: map[string]any{"file_path": path, "content": "x"},
		}}}
	}
	a, err := New(Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(
				messages.AI("plan"),         // assess
				writeCall("w1", "x.txt"),    // execute: first permission ask
				writeCall("w2", "y.txt"),    // after resume: second ask
				messages.AI("done writing"), // after second resume: finish
				messages.AI(`{"verdict":"complete","evidence":"files written"}`), // evaluate
			),
		),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGoal("write files", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	stream, err := a.GoalStream(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	first := pumpGoalChunks(t, stream)
	if len(first.interrupts) != 1 {
		t.Fatalf("first pause interrupts = %d (err %v)", len(first.interrupts), first.err)
	}

	// Resume once: the cycle asks again, so the resumed stream pauses too
	// (GoalResume's paused bookkeeping).
	resumed, err := a.GoalResume(context.Background(), "y")
	if err != nil {
		t.Fatal(err)
	}
	second := pumpGoalChunks(t, resumed)
	if len(second.interrupts) != 1 {
		t.Fatalf("second pause interrupts = %d (err %v)", len(second.interrupts), second.err)
	}
	if _, ok := second.interrupts[0].Value.(PermissionRequest); !ok {
		t.Fatalf("second interrupt value = %T", second.interrupts[0].Value)
	}

	// A consumer that stops pumping immediately must not wedge the loop.
	broken, err := a.GoalResume(context.Background(), "y")
	if err != nil {
		t.Fatal(err)
	}
	sawChunk := false
	for c, cerr := range broken {
		_ = c
		_ = cerr
		sawChunk = true
		break
	}
	if !sawChunk {
		t.Fatal("expected at least one chunk before breaking")
	}
	if a.GoalRunning() {
		t.Fatal("abandoned pump must release the goal-run bookkeeping")
	}
}
