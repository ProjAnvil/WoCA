package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/bus"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// newBypassAgent builds a bypass-permission agent over a fake model.
func newBypassAgent(t *testing.T, model language.ChatModel) *Agent {
	t.Helper()
	a, err := New(Config{
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

// baseChatModel strips woca's model wrappers (Observe/Router/namedModel)
// down to the configured model, for tests asserting the configuration
// identity behind the decorators the agent installs.
func baseChatModel(m language.ChatModel) language.ChatModel {
	for {
		u, ok := m.(interface{ Unwrap() language.ChatModel })
		if !ok || u.Unwrap() == nil {
			return m
		}
		m = u.Unwrap()
	}
}

func TestBackgroundTaskOutputBuffering(t *testing.T) {
	bt := &backgroundTask{status: TaskRunning}
	bt.appendOutput("")
	if _, out, _ := bt.snapshot(); out != "" {
		t.Fatalf("empty append must be a no-op, got %q", out)
	}
	bt.appendOutput("line1\n\nline2")
	if _, out, final := bt.snapshot(); out != "line1\nline2" || final != "" {
		t.Fatalf("snapshot = %q / %q", out, final)
	}
	// The ring keeps only the last 200 lines.
	var b strings.Builder
	for range 250 {
		b.WriteString("fill\n")
	}
	bt.appendOutput(b.String())
	_, out, _ := bt.snapshot()
	if strings.Count(out, "\n")+1 != 200 {
		t.Fatalf("kept %d lines, want 200", strings.Count(out, "\n")+1)
	}
	if strings.Contains(out, "line1") {
		t.Fatal("oldest lines were not trimmed from the ring")
	}
}

func TestBackgroundTaskFinalStateTransitions(t *testing.T) {
	bt := &backgroundTask{status: TaskRunning, done: make(chan struct{})}
	bt.setFinal(TaskCompleted, "done")
	if st, _, final := bt.snapshot(); st != TaskCompleted || final != "done" {
		t.Fatalf("setFinal = %v/%q", st, final)
	}
	select {
	case <-bt.done:
	default:
		t.Fatal("setFinal did not close done")
	}
	// A late completion must not flip a stopped task back.
	bt2 := &backgroundTask{status: TaskStopped}
	bt2.setFinalIfRunning(TaskCompleted, "late")
	if st, _, final := bt2.snapshot(); st != TaskStopped || final != "" {
		t.Fatalf("setFinalIfRunning overrode stop: %v/%q", st, final)
	}
	bt3 := &backgroundTask{status: TaskRunning, done: make(chan struct{})}
	bt3.setFinalIfRunning(TaskCompleted, "ok")
	if st, _, final := bt3.snapshot(); st != TaskCompleted || final != "ok" {
		t.Fatalf("setFinalIfRunning = %v/%q", st, final)
	}
	select {
	case <-bt3.done:
	default:
		t.Fatal("setFinalIfRunning did not close done")
	}
}

func TestBackgroundTaskStopCancels(t *testing.T) {
	bt := &backgroundTask{status: TaskRunning, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	bt.setCancel(cancel)
	bt.stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stop did not cancel the run context")
	}
	select {
	case <-bt.done:
	default:
		t.Fatal("stop did not close done")
	}
	if st, _, _ := bt.snapshot(); st != TaskStopped {
		t.Fatalf("status = %v", st)
	}
	// Stopping twice keeps the stopped status without a second done close; a
	// nil cancel is tolerated.
	bt.setCancel(nil)
	bt.stop()
	if st, _, _ := bt.snapshot(); st != TaskStopped {
		t.Fatalf("status after double stop = %v", st)
	}
}

// TestBackgroundTaskDoneClosedExactlyOnce races setFinalIfRunning against
// stop: done must end closed, and the losing transition must not close it a
// second time (close of a closed channel panics).
func TestBackgroundTaskDoneClosedExactlyOnce(t *testing.T) {
	for range 50 {
		bt := &backgroundTask{status: TaskRunning, done: make(chan struct{})}
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					bt.setFinalIfRunning(TaskCompleted, "done")
				} else {
					bt.stop()
				}
			}()
		}
		wg.Wait()
		select {
		case <-bt.done:
		default:
			t.Fatal("done not closed after concurrent finalization")
		}
	}
}

func TestTaskManagerStartGetList(t *testing.T) {
	m := newTaskManager()
	if _, ok := m.get("task-1"); ok {
		t.Fatal("empty manager should not find tasks")
	}
	if list := m.list(); len(list) != 0 {
		t.Fatalf("list = %v", list)
	}
	first, err := m.start(t.Context(), "subject", "description")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.start(t.Context(), "another", "d2")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "task-1" || second.ID != "task-2" {
		t.Fatalf("ids = %s %s", first.ID, second.ID)
	}
	if first.Subject != "subject" || first.Description != "description" {
		t.Fatalf("task = %+v", first)
	}
	got, ok := m.get("task-2")
	if !ok || got != second {
		t.Fatal("get(task-2) failed")
	}
	list := m.list()
	if len(list) != 2 || list[0].ID != "task-1" || list[1].ID != "task-2" {
		t.Fatalf("list not sorted by ID: %+v", list)
	}
	// start captures the starting run's session from ctx; "" outside a run
	// scope (completion notifications deliver there).
	scoped, err := m.start(tracing.WithRun(t.Context(), "r1", "sess-a"), "s3", "d3")
	if err != nil {
		t.Fatal(err)
	}
	if scoped.session != "sess-a" {
		t.Fatalf("scoped task session = %q", scoped.session)
	}
	if first.session != "" {
		t.Fatalf("unscoped task session = %q", first.session)
	}
	if scoped.done == nil {
		t.Fatal("started task has no done channel")
	}
}

func TestRunTaskRequiresDescriptionOrPrompt(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if _, err := a.runTask(t.Context(), taskArgs{}); err == nil {
		t.Fatal("empty task args must error")
	}
}

// gatedModel releases its single streamed response only when the test closes
// the gate, so background tasks stay observably "running".
type gatedModel struct {
	*language.FakeChatModel
	gate chan struct{}
}

func (g *gatedModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	if err := g.await(ctx); err != nil {
		return messages.Message{}, err
	}
	return messages.AI("gated report"), nil
}

func (g *gatedModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	if err := g.await(ctx); err != nil {
		return nil, err
	}
	return runnables.NewSliceStream([]messages.Message{messages.AI("gated report")}), nil
}

func (g *gatedModel) await(ctx context.Context) error {
	select {
	case <-g.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newBackgroundTaskAgent(t *testing.T, sub language.ChatModel) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
	)
	a, err := New(Config{
		Model:          model,
		SubagentModel:  sub,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunTaskBackgroundLifecycle(t *testing.T) {
	gate := make(chan struct{})
	sub := &gatedModel{FakeChatModel: language.NewFakeChatModel(), gate: gate}
	a := newBackgroundTaskAgent(t, sub)

	res, err := a.runTask(t.Context(), taskArgs{Description: "slow work", RunInBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Started background task task-1") {
		t.Fatalf("result = %q", res.Content)
	}

	// While the subagent is blocked the task reports running without output.
	out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "still running") {
		t.Fatalf("early output = %q", out.Content)
	}

	close(gate)
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
		return err == nil && out.Content == "gated report"
	}, "background task completion")

	// Unknown ids error on both endpoints.
	if _, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "nope"}); err == nil {
		t.Fatal("taskOutput on unknown id must error")
	}
	if _, err := a.taskStop(t.Context(), taskStopArgs{TaskID: "nope"}); err == nil {
		t.Fatal("taskStop on unknown id must error")
	}
}

func TestTaskStopReportsStopped(t *testing.T) {
	gate := make(chan struct{})
	sub := &gatedModel{FakeChatModel: language.NewFakeChatModel(), gate: gate}
	a := newBackgroundTaskAgent(t, sub)
	if _, err := a.runTask(t.Context(), taskArgs{Description: "x", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	res, err := a.taskStop(t.Context(), taskStopArgs{TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Stopped task task-1") {
		t.Fatalf("stop result = %q", res.Content)
	}
	out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "was stopped") {
		t.Fatalf("output after stop = %q", out.Content)
	}
	close(gate) // release the cancelled subagent so the test can end cleanly
}

// erroringSubagentModel fails every Invoke; the background wrapper reports
// "Error: ..." as the task's final output, and the foreground runSubagent
// path surfaces the error directly.
type erroringSubagentModel struct {
	*language.FakeChatModel
}

func (erroringSubagentModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	return messages.Message{}, errors.New("subagent exploded")
}

func (erroringSubagentModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return nil, errors.New("subagent exploded")
}

func TestRunTaskSubagentErrors(t *testing.T) {
	sub := erroringSubagentModel{language.NewFakeChatModel()}
	a := newBackgroundTaskAgent(t, sub)

	// Foreground: the error propagates.
	if _, err := a.runSubagent(t.Context(), []messages.Message{messages.Human("x")}, nil, nil); err == nil {
		t.Fatal("subagent error must propagate")
	}

	// Background: the error becomes the task's final report.
	if _, err := a.runTask(t.Context(), taskArgs{Description: "boom", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
		return err == nil && strings.Contains(out.Content, "Error: subagent exploded")
	}, "failed background task report")
}

// TestBackgroundSubagentInterruptStaysError locks the background Task path
// against the foreground interrupt relay: a background subagent's permission
// pause must NOT bubble onto the parent graph (the parent stream completes
// without an interrupt chunk) and the task must fail closed — the pending
// pause surfaces as an "Error: ..." final report (no one can answer a
// background pause) instead of silently completing, and the gated Write never
// executes. The parent model is transcript-driven: the Task tool returns
// immediately, so the scripted second response would replay the Task call.
func TestBackgroundSubagentInterruptStaysError(t *testing.T) {
	workdir := t.TempDir()
	a, err := New(Config{
		Model: &transcriptModel{script: []messages.Message{
			{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Task", Args: map[string]any{
					"description":       "write files",
					"prompt":            "write the files I asked for",
					"run_in_background": true,
				}},
			}},
			messages.AI("parent done"),
		}},
		SubagentModel: &transcriptModel{script: []messages.Message{
			writeCall("w1", "bg.txt", "hello"),
			messages.AI("sub done"),
		}},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, "thread-bg-relay"))
	if len(intrs) != 0 {
		t.Fatalf("parent paused on a background subagent's permission: %v", DecodeInterruptValue(intrs[0].Value))
	}
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(ctx, taskOutputArgs{TaskID: "task-1"})
		return err == nil && strings.HasPrefix(out.Content, "Error:")
	}, "background task error report")
	// The gated Write never executed: the pause is pending, not approved.
	if _, err := os.Stat(filepath.Join(workdir, "bg.txt")); !os.IsNotExist(err) {
		t.Fatalf("background Write executed despite the pending pause (stat err = %v)", err)
	}
}

func TestRunSubagentStreamsProgressToCallback(t *testing.T) {
	// A scripted subagent whose model streams tool use then a final message:
	// the onText callback must observe the tool-call line and text before the
	// final report lands.
	subModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "s1", Name: "Read", Args: map[string]any{"file_path": "x.go"}},
			}},
			messages.AI("streamed final report"),
		),
	)
	a := newBackgroundTaskAgent(t, subModel)
	if _, err := a.runTask(t.Context(), taskArgs{Description: "streamy", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1"})
		return err == nil && out.Content == "streamed final report"
	}, "streamed subagent final report")
}

func TestSubagentNameAndForkPrompt(t *testing.T) {
	if got := subagentName(nil, taskArgs{}); got != "general" {
		t.Fatalf("subagentName(nil) = %q", got)
	}
	if got := subagentName(nil, taskArgs{SubagentType: "typed"}); got != "typed" {
		t.Fatalf("subagentName(typed) = %q", got)
	}
	if got := subagentName(&agents.Definition{Name: "named"}, taskArgs{}); got != "named" {
		t.Fatalf("subagentName(named) = %q", got)
	}
	if !strings.Contains(forkTaskPrompt("do it"), "<task-fork>") {
		t.Fatal("fork prompt missing wrapper")
	}
}

func TestRunTaskForkUsesTranscript(t *testing.T) {
	subModel := language.NewFakeChatModel(language.WithResponses(messages.AI("forked report")))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("done")),
	)
	a, err := New(Config{
		Model:          model,
		SubagentModel:  subModel,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.setTranscript([]messages.Message{
		messages.System("parent system"),
		messages.Human("earlier context"),
	})
	res, err := a.runTask(t.Context(), taskArgs{Description: "continue in parallel", Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "forked report" {
		t.Fatalf("forked report = %q", res.Content)
	}
}

func TestRunTaskDefToolsRestriction(t *testing.T) {
	subModel := language.NewFakeChatModel(language.WithResponses(messages.AI("restricted report")))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("done")),
	)
	a, err := New(Config{
		Model:          model,
		SubagentModel:  subModel,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Agents:         []agents.Definition{{Name: "reader", Prompt: "read only", Tools: []string{"Read", "Grep"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	def, ok := agents.Find(a.agentsFor(t.Context()), "reader")
	if !ok {
		t.Fatal("reader definition missing")
	}
	sub, err := a.newSubagent(t.Context(), &def)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sub.ToolNames() {
		if name != "Read" && name != "Grep" {
			t.Fatalf("unexpected tool %q in restricted subagent: %v", name, sub.ToolNames())
		}
	}
	report, err := a.runSubagent(t.Context(), []messages.Message{messages.Human("x")}, nil, &def)
	if err != nil || report != "restricted report" {
		t.Fatalf("runSubagent = %q, %v", report, err)
	}
}

func TestSkillToolLookupAndList(t *testing.T) {
	a := &Agent{skills: []skills.Skill{{Name: "greet", Description: "say hi", Body: "Say hello to {who}."}}}
	list := a.skillTool()
	if len(list) != 1 {
		t.Fatalf("skill tool list = %d", len(list))
	}
	res, err := list[0].Invoke(t.Context(), map[string]any{"skill": "greet", "args": map[string]any{"who": "you"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Say hello to you.") {
		t.Fatalf("expand result = %q", res.Content)
	}
	if _, err := list[0].Invoke(t.Context(), map[string]any{"skill": "missing"}); err == nil {
		t.Fatal("unknown skill must error")
	}
	// No skills and no loader: the tool is omitted.
	bare := &Agent{}
	if got := bare.skillTool(); got != nil {
		t.Fatalf("bare agent skillTool = %v", got)
	}
	// A workspace loader alone also gates the tool in.
	loader := &Agent{skillsForFn: func(string) []skills.Skill { return nil }}
	if got := loader.skillTool(); len(got) != 1 {
		t.Fatalf("loader agent skillTool = %v", got)
	}
	if got := skillList([]skills.Skill{{Name: "n", Description: "d"}}); got != "- n: d\n" {
		t.Fatalf("skillList = %q", got)
	}
}

func TestTaskListToolEndpoints(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))

	// Empty list renders the placeholder.
	res, err := a.runTaskList(t.Context(), taskListArgs{})
	if err != nil || res.Content != "(no tasks)" {
		t.Fatalf("empty list = %q, %v", res.Content, err)
	}

	// Create two tasks; the second depends on the first.
	if _, err := a.taskCreate(t.Context(), taskCreateArgs{Subject: "first", Description: "d1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.taskCreate(t.Context(), taskCreateArgs{Subject: "second", DependsOn: []string{"task-1"}}); err != nil {
		t.Fatal(err)
	}
	res, err = a.runTaskList(t.Context(), taskListArgs{})
	if err != nil || !strings.Contains(res.Content, "[task-1] pending: first") {
		t.Fatalf("list = %q, %v", res.Content, err)
	}

	// Get formats a record; unknown ids error.
	res, err = a.taskGet(t.Context(), taskGetArgs{TaskID: "task-1"})
	if err != nil || !strings.Contains(res.Content, "first") {
		t.Fatalf("get = %q, %v", res.Content, err)
	}
	if _, err := a.taskGet(t.Context(), taskGetArgs{TaskID: "zzz"}); err == nil {
		t.Fatal("get on unknown id must error")
	}

	// Update mutates status/subject/description; unknown ids error.
	res, err = a.taskUpdate(t.Context(), taskUpdateArgs{TaskID: "task-1", Status: "completed", Subject: "first!", Description: "done d"})
	if err != nil || !strings.Contains(res.Content, "completed") {
		t.Fatalf("update = %q, %v", res.Content, err)
	}
	if rec, _ := a.currentTaskList().Get("task-1"); rec.Subject != "first!" || rec.Description != "done d" {
		t.Fatalf("update did not persist: %+v", rec)
	}
	if _, err := a.taskUpdate(t.Context(), taskUpdateArgs{TaskID: "zzz"}); err == nil {
		t.Fatal("update on unknown id must error")
	}

	// formatTaskRecord renders the owner when present.
	if got := formatTaskRecord(TaskRecord{ID: "task-1", Status: TaskRecordInProgress, Owner: "alice", Subject: "s"}); !strings.Contains(got, "(alice)") {
		t.Fatalf("formatTaskRecord = %q", got)
	}
}

func TestTaskCreateWakesTeamMembers(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	team.list.CreateDeps("existing", "", "", nil)
	// Pretend a member registered without running its loop: taskCreate must
	// still poke every listed member name.
	team.mu.Lock()
	team.order = []string{"alice"}
	team.mu.Unlock()
	if _, err := lead.taskCreate(t.Context(), taskCreateArgs{Subject: "new work"}); err != nil {
		t.Fatal(err)
	}
	// The poke channel for the (not spawned) member was signaled.
	select {
	case <-team.waitPoke("alice"):
	default:
		t.Fatal("taskCreate should poke team members")
	}
}

func TestSendMessageViaBusWithoutTeam(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Bus:            bus.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.sendMessageTool() == nil {
		t.Fatal("bus-wired agent should expose SendMessage")
	}
	if _, err := a.sendMessage(t.Context(), sendMessageArgs{Content: "hi"}); err == nil {
		t.Fatal("empty recipient must error")
	}
	res, err := a.sendMessage(t.Context(), sendMessageArgs{Recipient: "web", Content: "hello"})
	if err != nil || !strings.Contains(res.Content, "Sent message to web.") {
		t.Fatalf("send = %q, %v", res.Content, err)
	}
	found := false
	for _, m := range a.bus.All() {
		if m.To == "web" && m.Content == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bus messages = %+v", a.bus.All())
	}

	bare := &Agent{}
	if got := bare.sendMessageTool(); got != nil {
		t.Fatalf("busless agent sendMessageTool = %v", got)
	}
}

func TestRunTaskDescriptionFallsBackToPrompt(t *testing.T) {
	subModel := language.NewFakeChatModel(language.WithResponses(messages.AI("report")))
	a := newBackgroundTaskAgent(t, subModel)
	// Prompt empty -> description becomes the prompt; no subagent_type.
	res, err := a.runTask(t.Context(), taskArgs{Description: "only description"})
	if err != nil || res.Content != "report" {
		t.Fatalf("runTask = %q, %v", res.Content, err)
	}
}

func TestTaskManagerCap(t *testing.T) {
	m := newTaskManager()
	for i := range maxBackgroundTasks {
		if _, err := m.start(t.Context(), "s", "d"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := m.start(t.Context(), "s", "d"); err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("err = %v, want cap error", err)
	}
	if n := m.RunningCount(); n != maxBackgroundTasks {
		t.Fatalf("RunningCount = %d, want %d", n, maxBackgroundTasks)
	}
}

// slowSubagentModel delays every response so a background task stays
// observably running across a short TaskOutput wait.
type slowSubagentModel struct {
	*language.FakeChatModel
	delay time.Duration
}

func (s slowSubagentModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	time.Sleep(s.delay)
	return s.FakeChatModel.Invoke(ctx, input, opts...)
}

func (s slowSubagentModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	time.Sleep(s.delay)
	return s.FakeChatModel.Stream(ctx, input, opts...)
}

// TestBackgroundTaskCompletionNotification verifies the notify callback: a
// background task started in session "s1" completes and the callback receives
// ("s1", text) where the text names the task, its final status and
// description, and carries the report tail capped by
// tools.NotificationOutputCap (leading ellipsis, dropped head).
func TestBackgroundTaskCompletionNotification(t *testing.T) {
	report := "ZZHEAD" + strings.Repeat("a", 2950) + "VISIBLE-TAIL" // 2968 bytes, cap is 2000
	sub := language.NewFakeChatModel(language.WithResponses(messages.AI(report)))
	a := newBackgroundTaskAgent(t, sub)

	type notification struct{ session, text string }
	got := make(chan notification, 1)
	a.tasks.SetNotify(func(session, text string) { got <- notification{session, text} })

	ctx := tracing.WithRun(t.Context(), "run-1", "s1")
	if _, err := a.runTask(ctx, taskArgs{Description: "notify work", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n.session != "s1" {
			t.Fatalf("notify session = %q, want s1", n.session)
		}
		for _, want := range []string{"task-1", "completed", "notify work", "VISIBLE-TAIL", "…"} {
			if !strings.Contains(n.text, want) {
				t.Fatalf("notification missing %q: %q", want, n.text)
			}
		}
		if strings.Contains(n.text, "ZZHEAD") {
			t.Fatal("notification carries the uncapped report head")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completion notification never fired")
	}
}

// TestBackgroundTaskNotificationQueuedForSession locks the agent.New wiring:
// the manager's default notify callback queues the completion as a
// background-task-completed notification scoped to the starting session.
func TestBackgroundTaskNotificationQueuedForSession(t *testing.T) {
	sub := language.NewFakeChatModel(language.WithResponses(messages.AI("queued report body")))
	a := newBackgroundTaskAgent(t, sub)

	ctx := tracing.WithRun(t.Context(), "run-2", "s2")
	if _, err := a.runTask(ctx, taskArgs{Description: "queue me", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return a.QueuedInputCountFor("s2") == 1 },
		"task completion queued for session s2")
	items := a.DrainQueuedInputsFor("s2")
	if len(items) != 1 || items[0].Tag != "background-task-completed" || items[0].Session != "s2" {
		t.Fatalf("queued items = %+v", items)
	}
	if !strings.Contains(items[0].Text, "task-1") || !strings.Contains(items[0].Text, "queued report body") {
		t.Fatalf("notification text = %q", items[0].Text)
	}
}

// TestTaskOutputWaitMS covers TaskOutput's blocking wait: a wait shorter than
// the model delay reports the task still running; a wait longer than the
// delay returns the completed report in the same call, no second poll.
func TestTaskOutputWaitMS(t *testing.T) {
	sub := &slowSubagentModel{
		FakeChatModel: language.NewFakeChatModel(language.WithResponses(messages.AI("slow report"))),
		delay:         500 * time.Millisecond,
	}
	a := newBackgroundTaskAgent(t, sub)
	if _, err := a.runTask(t.Context(), taskArgs{Description: "slow", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}

	out, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1", WaitMS: 150})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "still running") {
		t.Fatalf("short-wait output = %q", out.Content)
	}

	out, err = a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1", WaitMS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	// The wait must have covered the model's remaining delay: a TaskOutput
	// that returned without blocking would still see "still running".
	if out.Content != "slow report" {
		t.Fatalf("blocking-wait output = %q", out.Content)
	}
}

// TestTaskStopWakesTaskOutputWaiterAndNotifies verifies both stop-side
// behaviors: closing done releases a blocked TaskOutput waiter promptly with
// the stopped status (far ahead of its wait), and the stopped task still
// notifies — with the stopped status line, never a silent drop.
func TestTaskStopWakesTaskOutputWaiterAndNotifies(t *testing.T) {
	gate := make(chan struct{})
	sub := &gatedModel{FakeChatModel: language.NewFakeChatModel(), gate: gate}
	a := newBackgroundTaskAgent(t, sub)

	notify := make(chan string, 1)
	a.tasks.SetNotify(func(session, text string) { notify <- text })

	if _, err := a.runTask(t.Context(), taskArgs{Description: "stoppable", RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	waited := make(chan string, 1)
	go func() {
		res, err := a.taskOutput(t.Context(), taskOutputArgs{TaskID: "task-1", WaitMS: 30000})
		if err != nil {
			waited <- "error: " + err.Error()
			return
		}
		waited <- res.Content
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter block first
	if _, err := a.taskStop(t.Context(), taskStopArgs{TaskID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case content := <-waited:
		if !strings.Contains(content, "was stopped") {
			t.Fatalf("released wait output = %q", content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskStop did not release the TaskOutput waiter")
	}
	select {
	case text := <-notify:
		if !strings.Contains(text, "task-1") || !strings.Contains(text, "stopped") {
			t.Fatalf("stopped notification = %q", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopped task never notified")
	}
	close(gate) // release the cancelled subagent so the test can end cleanly
}
