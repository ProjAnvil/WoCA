package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

// --- agent.go constructor and accessor gaps ---

func TestNewDefaultPromptModeAndLimits(t *testing.T) {
	a, err := New(Config{
		Model:    language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry: tools.NewRegistry(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.SystemPrompt() != "You are a helpful coding assistant." {
		t.Fatalf("default prompt = %q", a.SystemPrompt())
	}
	if a.Mode() != config.ModeDefault {
		t.Fatalf("default mode = %q", a.Mode())
	}
	if a.recursionLimit != 50 || a.workdir != "." {
		t.Fatalf("defaults = %d/%q", a.recursionLimit, a.workdir)
	}
}

// bindFailModel rejects tool binding.
type bindFailModel struct {
	*language.FakeChatModel
}

func (b *bindFailModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return nil, errors.New("bind boom")
}

func TestNewRouterAndBindErrors(t *testing.T) {
	// A retry/fallback router wraps the model transparently.
	a, err := New(Config{
		Model:           language.NewFakeChatModel(language.WithResponses(messages.AI("routed"))),
		Registry:        tools.NewRegistry(t.TempDir()),
		SystemPrompt:    "t",
		ModelMaxRetries: 1,
		FallbackModel:   language.NewFakeChatModel(language.WithResponses(messages.AI("fb"))),
		PermissionMode:  config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t")
	if err != nil || out[len(out)-1].Content != "routed" {
		t.Fatalf("routed reply = %q, %v", out[len(out)-1].Content, err)
	}

	// A model whose BindTools fails aborts construction.
	bad := &bindFailModel{language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}))}
	if _, err := New(Config{
		Model:          bad,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	}); err == nil {
		t.Fatal("bind failure must abort New")
	}
	// And a later SetModel with the same failure errors too.
	good := newBypassAgent(t, language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true})))
	if err := good.SetModel(bad); err == nil {
		t.Fatal("SetModel bind failure must error")
	}
}

func TestNewWithCheckpointer(t *testing.T) {
	a, err := New(Config{
		Model:        language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "t",
		Checkpointer: lgcheckpoint.NewMemorySaver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t"); err != nil {
		t.Fatal(err)
	}
}

func TestReducerRegistersNewIDs(t *testing.T) {
	out := MessagesReducer(nil, []messages.Message{
		{ID: "x", Role: messages.RoleHuman, Content: "1"},
		{ID: "x", Role: messages.RoleHuman, Content: "replaced"},
		{ID: "y", Role: messages.RoleHuman, Content: "2"},
	}).([]messages.Message)
	if len(out) != 2 || out[0].Content != "replaced" || out[1].Content != "2" {
		t.Fatalf("reducer = %+v", out)
	}
}

func TestSetMemoryEngineAccessor(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if a.MemoryEngine() != nil {
		t.Fatal("engine should start nil")
	}
	f := &fakeEngine{}
	a.SetMemoryEngine(f)
	if a.MemoryEngine() != f {
		t.Fatal("SetMemoryEngine did not take effect")
	}
	a.SetMemoryEngine(nil)
	if a.MemoryEngine() != nil {
		t.Fatal("SetMemoryEngine(nil) did not clear")
	}
}

func TestPostCompactHookRunsOnSuccess(t *testing.T) {
	ran := false
	runner := hooks.NewRunner(hooks.Config{
		PostCompact: []hooks.Entry{{Hooks: []hooks.Hook{{Command: "after"}}}},
	}, ".", "s1", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		ran = true
		return hooks.CommandResult{Stdout: "compacted"}, nil
	}))
	a, err := New(Config{
		Model:            language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:         tools.NewRegistry(t.TempDir()),
		SystemPrompt:     "t",
		PermissionMode:   config.ModeBypass,
		CompactThreshold: 10,
		Summarizer:       language.NewFakeChatModel(language.WithResponses(messages.AI("summary"))),
		Hooks:            runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := a.ForceCompact(t.Context(), bigHistory())
	if len(out) >= len(bigHistory()) {
		t.Fatalf("compaction did not shrink: %d", len(out))
	}
	if !ran {
		t.Fatal("PostCompact hook did not run")
	}
}

// --- toolsNode / gate integration gaps ---

func TestToolsNodeNoopStates(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	node := a.toolsNode()
	// Empty state: nothing to do.
	rt := lgruntime.NewRuntime(t.Context())
	v, err := node(rt, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("empty state update = %v", v)
	}
	// Last message is not an AI tool request: nothing to do.
	v, err = node(rt, map[string]any{"messages": []messages.Message{messages.Human("hi")}})
	if err != nil || v != nil {
		t.Fatalf("human-tail state = %v, %v", v, err)
	}
	// Route agrees: an empty state ends the turn.
	next, err := a.route(rt, map[string]any{})
	if err != nil || len(next) != 1 || next[0] != lgtypes.END {
		t.Fatalf("route(empty) = %v, %v", next, err)
	}
}

// gatedWriteModel issues a Write tool call, then finishes.
func gatedWriteModel(final string) language.ChatModel {
	return language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI(final),
		),
	)
}

func TestToolsNodeHookFeedbackAndDeny(t *testing.T) {
	runner := hooks.NewRunner(hooks.Config{
		PreToolUse:  []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Command: "pre"}}}},
		PostToolUse: []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Command: "post"}}}},
	}, ".", "s1", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		if command == "pre" {
			return hooks.CommandResult{Stdout: "pre-note"}, nil
		}
		return hooks.CommandResult{Stdout: "post-note"}, nil
	}))
	a, err := New(Config{
		Model:          gatedWriteModel("wrote with feedback"),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"Write"}},
		Hooks:          runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == "w" {
			if !strings.Contains(m.Content, "pre-note") || !strings.Contains(m.Content, "post-note") {
				t.Fatalf("hook feedback missing from tool result: %q", m.Content)
			}
		}
	}
}

func TestToolsNodePreToolUseHookDenyInGate(t *testing.T) {
	runner := hooks.NewRunner(hooks.Config{
		PreToolUse: []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Command: "deny"}}}},
	}, ".", "s1", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		return hooks.CommandResult{ExitCode: 2, Stdout: "denied by policy"}, nil
	}))
	a, err := New(Config{
		Model:          gatedWriteModel("after denial"),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"Write"}},
		Hooks:          runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == "w" && strings.Contains(m.Content, "denied by policy") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denial missing from transcript: %+v", out)
	}
}

func TestPermissionDeniedOnResume(t *testing.T) {
	a, err := New(Config{
		Model:          gatedWriteModel("should not run"),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("write")}, "t")
	_ = interruptFromError(t, err)
	state, err := a.Resume(t.Context(), "t", "n")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range state {
		if m.Role == messages.RoleTool && m.ToolCallID == "w" && strings.Contains(m.Content, "permission denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied result missing: %+v", state)
	}
}

func TestPlanRejectionAndAskRuleInterrupt(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "p", Name: "EnterPlanMode", Args: map[string]any{}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "x", Name: "ExitPlanMode", Args: map[string]any{"plan": "the plan"}},
			}},
			messages.AI("revising"),
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
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("plan")}, "t")
	_ = interruptFromError(t, err)
	state, err := a.Resume(t.Context(), "t", "n")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range state {
		if m.Role == messages.RoleTool && m.ToolCallID == "x" && strings.Contains(m.Content, "Plan rejected") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejection missing: %+v", state)
	}

	// An ask rule in default mode interrupts (outside a graph run it errors).
	asking := &Agent{mode: config.ModeDefault}
	asking.perms = &permissions.Rules{Ask: []string{"Write"}}
	askOutsideRun(t, func() {
		_, _ = asking.checkPermission(t.Context(), messages.ToolCall{
			Name: "Write", Args: map[string]any{"file_path": "x"},
		})
	})
	// Bypass allows everything; default auto-allows reads.
	byp := &Agent{mode: config.ModeBypass}
	if ok, err := byp.checkPermission(t.Context(), messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "anything"}}); err != nil || !ok {
		t.Fatalf("bypass = %v/%v", ok, err)
	}
	def := &Agent{mode: config.ModeDefault}
	if ok, err := def.checkPermission(t.Context(), messages.ToolCall{Name: "Read", Args: map[string]any{"file_path": "x"}}); err != nil || !ok {
		t.Fatalf("default read = %v/%v", ok, err)
	}
}

func TestMaxTurnsMarkerEndsTurn(t *testing.T) {
	// Each model round asks for one read; with MaxTurns=1 the single tool
	// round appends the marker and short-circuits to END.
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "r", Name: "Read", Args: map[string]any{"file_path": "agent.go"}},
		}}),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry("."),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		MaxTurns:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("read")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	last := out[len(out)-1]
	if !strings.Contains(last.Content, "<max-turns-reached>") || !strings.Contains(last.Content, "1 model round") {
		t.Fatalf("marker missing: %+v", last)
	}
}

func TestDecodeArgsMarshalError(t *testing.T) {
	var out struct{ X int }
	if err := decodeArgs(map[string]any{"x": make(chan int)}, &out); err == nil {
		t.Fatal("unmarshalable args must error")
	}
}

func TestNewSubagentBuildError(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	a.subagentModel = nil
	if _, err := a.newSubagent(t.Context(), nil); err == nil {
		t.Fatal("nil subagent model must fail the build")
	}
}

// --- task.go gaps ---

func TestRunTaskForegroundErrorAndTypedSubagent(t *testing.T) {
	// Foreground error path: the subagent model fails.
	failing := newBackgroundTaskAgent(t, erroringSubagentModel{language.NewFakeChatModel()})
	if _, err := failing.runTask(t.Context(), taskArgs{Description: "boom"}); err == nil {
		t.Fatal("foreground task error must propagate")
	}

	// A valid subagent_type resolves the definition and runs it.
	sub := language.NewFakeChatModel(language.WithResponses(messages.AI("typed report")))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}))
	a, err := New(Config{
		Model:          model,
		SubagentModel:  sub,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Agents:         []agents.Definition{{Name: "explorer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.runTask(t.Context(), taskArgs{Description: "explore", SubagentType: "explorer"})
	if err != nil || res.Content != "typed report" {
		t.Fatalf("typed task = %q, %v", res.Content, err)
	}
}

func TestRunTaskBackgroundBuildError(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true})))
	a.subagentModel = nil
	if _, err := a.runTask(t.Context(), taskArgs{Description: "x", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
		return err == nil && strings.HasPrefix(out.Content, "Error: ")
	}, "background build failure report")
}

// trickleModel completes its first round (a read-only tool call, whose AI
// update lands in the task's output via the onText callback), then blocks its
// second round until the test releases the gate.
type trickleModel struct {
	*language.FakeChatModel
	gate   chan struct{}
	rounds int
}

func (m *trickleModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.rounds++
	if m.rounds == 1 {
		return runnables.NewSliceStream([]messages.Message{
			{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Read", Args: map[string]any{"file_path": "x.go"}},
			}},
		}), nil
	}
	select {
	case <-m.gate:
		return runnables.NewSliceStream([]messages.Message{messages.AI("trickle done")}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestTaskOutputWhileRunningShowsProgress(t *testing.T) {
	gate := make(chan struct{})
	sub := &trickleModel{FakeChatModel: language.NewFakeChatModel(), gate: gate}
	a := newBackgroundTaskAgent(t, sub)
	if _, err := a.runTask(t.Context(), taskArgs{Description: "slow", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
		return err == nil && strings.Contains(out.Content, "still running. Output so far")
	}, "partial output while running")
	close(gate)
}

func TestTaskUpdateClaimsInTeamScope(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	first := team.list.CreateDeps("first", "", "", nil)
	blocked := team.list.CreateDeps("blocked", "", "", []string{first.ID})
	lead.agentName = "worker"

	// An unowned pending task is claimed by moving it to in_progress.
	res, err := lead.taskUpdate(t.Context(), taskUpdateArgs{TaskID: first.ID, Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "in_progress") || !strings.Contains(res.Content, "worker") {
		t.Fatalf("claim result = %q", res.Content)
	}
	if rec, _ := team.list.Get(first.ID); rec.Owner != "worker" || rec.Status != TaskRecordInProgress {
		t.Fatalf("claimed record = %+v", rec)
	}
	// A dependency-blocked task cannot be claimed.
	if _, err := lead.taskUpdate(t.Context(), taskUpdateArgs{TaskID: blocked.ID, Status: "in_progress"}); err == nil {
		t.Fatal("dependency-blocked claim must error")
	}
	// The lead's own update path (no claim transition) still works.
	lead.agentName = LeadName
	if _, err := lead.taskUpdate(t.Context(), taskUpdateArgs{TaskID: first.ID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
}

// --- goalgraph.go gaps ---

func TestGoalPauseMidLoopEndsAtBoundary(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.AI("work 1"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`),
		messages.AI("assess 2"),
		messages.AI("work 2"),
		messages.AI(`{"verdict":"complete","evidence":"done"}`),
	)
	a.SetGoal("pausable", "", 10, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var termErr error
	for c, cerr := range stream {
		if cerr != nil {
			termErr = cerr
			break
		}
		if c.Mode == lggraph.StreamCustom {
			if note, ok := c.Payload.(GoalNote); ok && strings.Contains(note.Text, "[goal cycle") {
				a.PauseGoal() // user pauses mid-loop
			}
		}
	}
	if termErr != nil {
		t.Fatalf("paused loop should end cleanly, got %v", termErr)
	}
	if g := a.Goal(); g.Status != GoalPaused {
		t.Fatalf("status = %q, want paused", g.Status)
	}
}

func TestGoalAssessRendersNotesAndEmptyAssessment(t *testing.T) {
	// Cycle 1 records a progress note; cycle 2's assess prompt must render it.
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "n1", Name: "UpdateGoal",
			Args: map[string]any{"action": "progress", "note": "halfway through"},
		}}},
		messages.AI("worked"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`),
		messages.AI(""), // cycle 2 assess: empty output falls back
		messages.AI("worked again"),
		messages.AI(`{"verdict":"complete","evidence":"done"}`),
	)
	a.SetGoal("noted", "", 5, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	restep := pumpGoalChunks(t, stream)
	if restep.err != nil || len(restep.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", restep.err)
	}
	g := a.Goal()
	if g.Status != GoalComplete {
		t.Fatalf("status = %q (%+v)", g.Status, g)
	}
	if len(g.Notes) != 1 || g.Notes[0] != "halfway through" {
		t.Fatalf("notes = %v", g.Notes)
	}
}

func TestGoalEvaluatorBlockedVerdict(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess"),
		messages.AI("work"),
		messages.AI(`{"verdict":"blocked","gaps":"needs human input"}`),
	)
	a.SetGoal("blocked goal", "", 5, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	restep := pumpGoalChunks(t, stream)
	if restep.err != nil || len(restep.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", restep.err)
	}
	g := a.Goal()
	if g.Status != GoalBlocked || g.BlockedReason != "needs human input" {
		t.Fatalf("goal = %+v", g)
	}
}

// goalCountingModel serves a fixed list of answers, then errors.
type goalCountingModel struct {
	*language.FakeChatModel
	answers []messages.Message
	next    int
}

func (m *goalCountingModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	if m.next < len(m.answers) {
		a := m.answers[m.next]
		m.next++
		return a, nil
	}
	return messages.Message{}, errors.New("model exhausted")
}

func (m *goalCountingModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	a, err := m.Invoke(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return runnables.NewSliceStream([]messages.Message{a}), nil
}

func TestGoalEvaluateModelErrorSurfaces(t *testing.T) {
	// Assess and execute succeed; the evaluator call fails.
	a := newGoalAgent(t,
		messages.AI("assess"),
		messages.AI("work"),
		messages.AI(`{"verdict":"continue"}`))
	// Replace the runtime model with the counting one (2 answers then error).
	a.mu.Lock()
	a.boundModel = &goalCountingModel{answers: []messages.Message{
		messages.AI("assess"),
		messages.AI("work"),
	}}
	a.mu.Unlock()
	a.SetGoal("eval fails", "", 5, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	term := pumpError(t, stream)
	if term == nil {
		t.Fatal("expected the evaluate error to surface")
	}
	if a.GoalRunning() {
		t.Fatal("run not ended after the error")
	}
}

// pumpError drains a goal chunk stream and returns the terminal error (nil
// when the run completes cleanly).
func pumpError(t *testing.T, s iter.Seq2[lggraph.StreamChunk, error]) error {
	t.Helper()
	for _, err := range s {
		if err != nil {
			return err
		}
	}
	return nil
}

func TestGoalExecuteStreamErrorSurfaces(t *testing.T) {
	// Assess succeeds; the execute cycle's model stream fails.
	a := newGoalAgent(t, messages.AI("assess"), messages.AI("work"), messages.AI(`{}`))
	a.mu.Lock()
	a.boundModel = &goalCountingModel{answers: []messages.Message{messages.AI("assess")}}
	a.mu.Unlock()
	a.SetGoal("exec fails", "", 5, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pumpError(t, stream); err == nil {
		t.Fatal("expected the execute error to surface")
	}
	if a.GoalRunning() {
		t.Fatal("run not ended after the error")
	}
}

func TestGoalReasoningDeltaForwarded(t *testing.T) {
	// The execute cycle's model streams a reasoning block before its answer;
	// the goal stream must forward it as a ReasoningDelta.
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("assess"),
			messages.AI("done"),
			messages.AI(`{"verdict":"complete","evidence":"ok"}`),
		),
		language.WithStreamChunks(
			messages.Message{ContentBlocks: []messages.ContentBlock{messages.ReasoningBlock{Reasoning: "pondering"}}},
			messages.AI("done"),
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
	a.SetGoal("think it through", "", 5, 1_000_000)
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sawReasoning := false
	for c, cerr := range stream {
		if cerr != nil {
			t.Fatalf("stream error: %v", cerr)
		}
		if _, isR := c.Payload.(ReasoningDelta); isR {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatal("reasoning delta not forwarded by the goal stream")
	}
}

func TestGoalProgressWindowTrimsAtFifty(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess"),
		messages.AI("work"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`))
	a.SetGoal("long history", "", 1, 1_000_000)
	// Pre-fill 50 progress entries; the evaluate append pushes to 51 and trims.
	a.goal.mu.Lock()
	a.goal.goal.Progress = make([]GoalProgressEntry, 50)
	for i := range a.goal.goal.Progress {
		a.goal.goal.Progress[i] = GoalProgressEntry{Cycle: i + 1, Verdict: "continue"}
	}
	a.goal.mu.Unlock()
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	restep := pumpGoalChunks(t, stream)
	if restep.err != nil || len(restep.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", restep.err)
	}
	if g := a.Goal(); len(g.Progress) != 50 {
		t.Fatalf("progress = %d, want trimmed to 50", len(g.Progress))
	}
}

// --- team.go / memorystore.go / goal.go stragglers ---

func TestTeamStatusRendersUnownedTasks(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	team.list.CreateDeps("unowned work", "", "", nil)
	if got := team.Status(); !strings.Contains(got, "owner=-") {
		t.Fatalf("unowned task not rendered with '-':\n%s", got)
	}
}

func TestGoalSubscribeUnsubscribeKeepsOthers(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	var sawA, sawB bool
	unsubA := a.SubscribeGoal(func(*Goal) { sawA = true })
	a.SubscribeGoal(func(*Goal) { sawB = true })
	unsubA()
	a.SetGoal("kept", "", 1, 1)
	if sawA {
		t.Fatal("unsubscribed listener still notified")
	}
	if !sawB {
		t.Fatal("remaining listener not notified")
	}
}

func TestParseGoalCommandExplicitStatus(t *testing.T) {
	if cmd, err := ParseGoalCommand([]string{"status"}); err != nil || cmd.Verb != "status" {
		t.Fatalf("status = %+v, %v", cmd, err)
	}
}

func TestFormatGoalShortProgressWindow(t *testing.T) {
	g := &Goal{
		Objective: "o", Status: GoalActive,
		Progress: []GoalProgressEntry{{Cycle: 1, Verdict: "continue", Summary: "only"}},
	}
	if got := FormatGoal(g); !strings.Contains(got, "only") {
		t.Fatalf("short window render = %q", got)
	}
}

// --- worktree.go error paths ---

func TestWorktreeErrorPaths(t *testing.T) {
	a := newWorktreeAgent(t)
	ctx := t.Context()
	enter := a.toolByName["EnterWorktree"]
	exit := a.toolByName["ExitWorktree"]

	// A sanitized-empty name is rejected.
	if res, err := enter.Invoke(ctx, map[string]any{"name": "//"}); err != nil || !strings.Contains(res.Content, "name is required") {
		t.Fatalf("bad name = %q, %v", res.Content, err)
	}
	// Exiting without entering reports it.
	if res, err := exit.Invoke(ctx, map[string]any{}); err != nil || !strings.Contains(res.Content, "Not currently in a worktree") {
		t.Fatalf("bare exit = %q, %v", res.Content, err)
	}
	// A bad ref fails the git command without entering.
	if res, err := enter.Invoke(ctx, map[string]any{"name": "br", "ref": "no-such-ref"}); err != nil || !strings.Contains(res.Content, "Error: git worktree add") {
		t.Fatalf("bad ref = %q, %v", res.Content, err)
	}
	// Sanitizer keeps alnum/-/_ and drops everything else.
	if got := strings.Map(sanitizeWorktreeRune, "a B-c_9!"); got != "aB-c_9" {
		t.Fatalf("sanitize = %q", got)
	}
}

// --- memorystore.go stragglers ---

func TestBeginEmptyTermsNoop(t *testing.T) {
	kv := store.NewMemory()
	m := memoryRuntime{}
	m.init(kv, nil, nil, nil)
	// A human message with no long-enough terms: no prefetch starts.
	m.Begin(t.Context(), []messages.Message{messages.Human("hi")}, "/ws")
}

func TestTakeAttachmentsInjectedAndLongEntries(t *testing.T) {
	m := memoryRuntime{}
	long := strings.Repeat("z", 3000)
	m.mu.Lock()
	m.prefetch.entries = []store.Entry{
		{Namespace: "ns", Key: "dup", Value: []byte(`"first"`)},
		{Namespace: "ns", Key: "dup", Value: []byte(`"second"`)}, // duplicate id: skipped
		{Namespace: "ns", Key: "blank", Value: nil},              // empty value: skipped
		{Namespace: "ns", Key: "long", Value: []byte(long)},
	}
	m.mu.Unlock()
	out := m.TakeAttachments(t.Context())
	if len(out) != 2 {
		t.Fatalf("attachments = %d: %+v", len(out), out)
	}
	var longLine string
	for _, m := range out {
		if strings.Contains(m.Content, "ns/long") {
			longLine = m.Content
		}
	}
	if longLine == "" || !strings.HasSuffix(longLine, "…\n</relevant-memory>") {
		t.Fatalf("long entry not clamped: %q", longLine)
	}
}

func TestTakeAttachmentsEngineHitClampAndDup(t *testing.T) {
	m := memoryRuntime{}
	huge := strings.Repeat("y", 3000)
	m.mu.Lock()
	m.prefetch.engineHits = []memory.Hit{
		{ID: "h1", Layer: "L2", Type: "fact", Summary: "first", Content: huge},
		{ID: "h1", Layer: "L2", Type: "fact", Summary: "dup"}, // injected: skipped
	}
	m.mu.Unlock()
	out := m.TakeAttachments(t.Context())
	if len(out) != 1 {
		t.Fatalf("attachments = %d", len(out))
	}
	if !strings.Contains(out[0].Content, "first") {
		t.Fatalf("attachment = %q", out[0].Content)
	}
	if len(out[0].Content) > 2100+60 {
		t.Fatalf("content not clamped: %d", len(out[0].Content))
	}
}
