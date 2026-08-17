package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

func TestNewRequiresModelAndRegistry(t *testing.T) {
	if _, err := New(Config{Registry: tools.NewRegistry(t.TempDir())}); err == nil {
		t.Fatal("nil model must error")
	}
	if _, err := New(Config{Model: language.NewFakeChatModel()}); err == nil {
		t.Fatal("nil registry must error")
	}
}

func TestMessagesReducerAndFrom(t *testing.T) {
	base := []messages.Message{
		{ID: "a", Role: messages.RoleHuman, Content: "1"},
		{ID: "b", Role: messages.RoleAI, Content: "old"},
	}
	// Same-ID update replaces in place; new messages append.
	out := MessagesReducer(base, []messages.Message{
		{ID: "b", Role: messages.RoleAI, Content: "new"},
		messages.Human("tail"),
	}).([]messages.Message)
	if len(out) != 3 || out[1].Content != "new" || out[2].Content != "tail" {
		t.Fatalf("reducer result = %+v", out)
	}
	// nil inputs are tolerated.
	if got := MessagesReducer(nil, nil).([]messages.Message); len(got) != 0 {
		t.Fatalf("nil reducer = %+v", got)
	}
	if MessagesFrom(nil) != nil {
		t.Fatal("MessagesFrom(nil) must be nil")
	}
	if MessagesFrom(map[string]any{"other": 1}) != nil {
		t.Fatal("MessagesFrom on messageless state must be nil")
	}
}

func TestDoctorAccessors(t *testing.T) {
	hr := hooks.NewRunner(hooks.Config{
		PreToolUse: []hooks.Entry{{Matcher: "Bash", Hooks: []hooks.Hook{{Command: "x"}}}},
	}, ".", "s")
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Hooks:          hr,
		Permissions:    &permissions.Rules{Allow: []string{"Read"}, Deny: []string{"Bash"}, Ask: []string{"Write"}},
		Agents:         []agents.Definition{{Name: "explorer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.HookSummary(); len(got) == 0 {
		t.Fatal("HookSummary empty with hooks wired")
	}
	allow, deny, ask := a.PermissionRuleCounts()
	if allow != 1 || deny != 1 || ask != 1 {
		t.Fatalf("rule counts = %d/%d/%d", allow, deny, ask)
	}
	if len(a.AgentDefinitions()) != 1 {
		t.Fatalf("AgentDefinitions = %+v", a.AgentDefinitions())
	}
	a.SetAgentDefinitions([]agents.Definition{{Name: "second"}})
	if a.AgentDefinitions()[0].Name != "second" {
		t.Fatalf("SetAgentDefinitions = %+v", a.AgentDefinitions())
	}

	// Zero cases.
	bare := &Agent{}
	if a := bare; a.HookSummary() != nil {
		t.Fatal("HookSummary must be nil without a runner")
	}
	if allow, deny, ask := bare.PermissionRuleCounts(); allow != 0 || deny != 0 || ask != 0 {
		t.Fatalf("bare rule counts = %d/%d/%d", allow, deny, ask)
	}
}

func TestWorkspaceScopedResolvers(t *testing.T) {
	skillLoads, agentLoads, hookLoads := 0, 0, 0
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:        model,
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test",
		SkillsFor: func(ws string) []skills.Skill {
			skillLoads++
			return []skills.Skill{{Name: "s-" + ws, Body: "b"}}
		},
		AgentsFor: func(ws string) []agents.Definition {
			agentLoads++
			return []agents.Definition{{Name: "a-" + ws}}
		},
		HooksFor: func(ws string) *hooks.Runner {
			hookLoads++
			return hooks.NewRunner(hooks.Config{}, ws, "s")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// No workspace in ctx: the startup values apply.
	plain := context.Background()
	if got := a.skillsFor(plain); len(got) != 0 {
		t.Fatalf("skillsFor without ws = %+v", got)
	}
	if got := a.agentsFor(plain); got != nil {
		t.Fatalf("agentsFor without ws = %+v", got)
	}
	if got := a.hooksFor(plain); got != nil {
		t.Fatalf("hooksFor without ws = %+v", got)
	}

	// A run workspace selects and caches the loader results.
	wsCtx := runws.With(context.Background(), "/ws/proj")
	if got := a.skillsFor(wsCtx); len(got) != 1 || got[0].Name != "s-/ws/proj" {
		t.Fatalf("skillsFor = %+v", got)
	}
	if got := a.skillsFor(wsCtx); got[0].Name != "s-/ws/proj" {
		t.Fatalf("skillsFor cache miss: %+v", got)
	}
	if got := a.agentsFor(wsCtx); len(got) != 1 || got[0].Name != "a-/ws/proj" {
		t.Fatalf("agentsFor = %+v", got)
	}
	if got := a.agentsFor(wsCtx); got[0].Name != "a-/ws/proj" {
		t.Fatalf("agentsFor cache miss: %+v", got)
	}
	if got := a.hooksFor(wsCtx); got == nil {
		t.Fatal("hooksFor returned nil for workspace")
	}
	if got := a.hooksFor(wsCtx); got == nil {
		t.Fatal("hooksFor cache miss")
	}
	if skillLoads != 1 || agentLoads != 1 || hookLoads != 1 {
		t.Fatalf("loaders ran %d/%d/%d times, want 1 each (cache)", skillLoads, agentLoads, hookLoads)
	}

	// The exported wrappers delegate.
	if got := a.SkillsIn(wsCtx); got[0].Name != "s-/ws/proj" {
		t.Fatalf("SkillsIn = %+v", got)
	}
	if got := a.AgentDefinitionsIn(wsCtx); got[0].Name != "a-/ws/proj" {
		t.Fatalf("AgentDefinitionsIn = %+v", got)
	}
}

func TestSystemPromptBuilderAndInvalidation(t *testing.T) {
	builds := 0
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:        model,
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "static",
		SystemPromptFor: func(ws, mdl string) string {
			builds++
			return "prompt for " + ws + " model " + mdl
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runws.With(context.Background(), "/ws/alpha")
	if got := a.systemPromptFor(ctx); got != "prompt for /ws/alpha model *language.FakeChatModel" && !strings.HasPrefix(got, "prompt for /ws/alpha") {
		t.Fatalf("built prompt = %q", got)
	}
	a.systemPromptFor(ctx) // cached
	if builds != 1 {
		t.Fatalf("builder ran %d times, want 1 (cache)", builds)
	}

	// SetSystemPrompt with a builder wired only invalidates the cache.
	a.SetSystemPrompt("")
	if a.SystemPrompt() != "static" {
		t.Fatalf("empty swap must not change the prompt, got %q", a.SystemPrompt())
	}
	a.SetSystemPrompt("changed")
	builds = 0
	a.systemPromptFor(ctx)
	if builds != 1 {
		t.Fatalf("cache not invalidated by SetSystemPrompt (%d builds)", builds)
	}

	// Without a builder the swap applies directly.
	b := newBypassAgent(t, model)
	if b.SystemPrompt() != "test" {
		t.Fatalf("prompt = %q", b.SystemPrompt())
	}
	b.SetSystemPrompt("swapped")
	if b.SystemPrompt() != "swapped" {
		t.Fatalf("prompt after swap = %q", b.SystemPrompt())
	}
}

func TestSessionHookRunners(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	runner := hooks.NewRunner(hooks.Config{
		Stop:             []hooks.Entry{{Hooks: []hooks.Hook{{Command: "stop"}}}},
		UserPromptSubmit: []hooks.Entry{{Hooks: []hooks.Hook{{Command: "ups"}}}},
		SessionStart:     []hooks.Entry{{Hooks: []hooks.Hook{{Command: "start"}}}},
		SessionEnd:       []hooks.Entry{{Hooks: []hooks.Hook{{Command: "end"}}}},
	}, ".", "s1", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		return hooks.CommandResult{Stdout: "hook said " + command}, nil
	}))
	a, err := New(Config{
		Model:        model,
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test",
		Hooks:        runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb, _, err := a.RunStopHooks(context.Background(), "end_turn"); err != nil || fb == "" {
		t.Fatalf("RunStopHooks = %q, %v", fb, err)
	}
	if ctx, err := a.RunUserPromptSubmit(context.Background(), "hello"); err != nil || ctx == "" {
		t.Fatalf("RunUserPromptSubmit = %q, %v", ctx, err)
	}
	if fb, err := a.RunSessionStart(context.Background()); err != nil || fb == "" {
		t.Fatalf("RunSessionStart = %q, %v", fb, err)
	}
	if err := a.RunSessionEnd(context.Background(), "exit"); err != nil {
		t.Fatalf("RunSessionEnd = %v", err)
	}

	// Without a runner everything is a no-op.
	bare := newBypassAgent(t, model)
	if fb, blocked, err := bare.RunStopHooks(context.Background(), "r"); fb != "" || blocked || err != nil {
		t.Fatalf("bare RunStopHooks = %q/%v/%v", fb, blocked, err)
	}
	if ctx, err := bare.RunUserPromptSubmit(context.Background(), "p"); ctx != "" || err != nil {
		t.Fatalf("bare RunUserPromptSubmit = %q/%v", ctx, err)
	}
	if fb, err := bare.RunSessionStart(context.Background()); fb != "" || err != nil {
		t.Fatalf("bare RunSessionStart = %q/%v", fb, err)
	}
	if err := bare.RunSessionEnd(context.Background(), "exit"); err != nil {
		t.Fatalf("bare RunSessionEnd = %v", err)
	}
}

func TestObsAccessors(t *testing.T) {
	bare := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if bare.Obs() != nil {
		t.Fatal("Obs should be nil when unwired")
	}
	bare.SetObsRun("/ws", "sess", "run") // nil-safe

	obs := obslog.New(t.TempDir(), false)
	a, err := New(Config{
		Model:        language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test",
		Obs:          obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Obs() != obs {
		t.Fatal("Obs did not round-trip")
	}
	a.SetObsRun("/ws/proj", "sess", "run") // must not panic while disabled
}

func TestStateAccessorsAndMode(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	if a.Mode() != config.ModeBypass {
		t.Fatalf("mode = %q", a.Mode())
	}
	a.SetMode(config.ModePlan)
	if a.Mode() != config.ModePlan {
		t.Fatalf("mode after SetMode = %q", a.Mode())
	}
	a.SetPlanMode(true)
	if !a.PlanMode() {
		t.Fatal("SetPlanMode(true) lost")
	}
	a.SetPlanMode(false)
	if a.PlanMode() {
		t.Fatal("SetPlanMode(false) lost")
	}
	a.SetTodos([]tools.Todo{{Content: "x", Status: "pending"}})
	if todos := a.Todos(); len(todos) != 1 || todos[0].Content != "x" {
		t.Fatalf("todos = %+v", todos)
	}
	a.SetTasks([]TaskRecord{{ID: "task-7", Subject: "restored", Status: TaskRecordPending}})
	recs := a.Tasks()
	if len(recs) != 1 || recs[0].ID != "task-7" {
		t.Fatalf("tasks = %+v", recs)
	}
	// The next Create continues after the restored counter.
	a.currentTaskList().Create("next", "", "")
	if rec, _ := a.currentTaskList().Get("task-8"); rec.Subject != "next" {
		t.Fatalf("counter not recomputed on restore: %+v", rec)
	}
	a.SetSkills([]skills.Skill{{Name: "hot"}})
	if sk := a.Skills(); len(sk) != 1 || sk[0].Name != "hot" {
		t.Fatalf("skills = %+v", sk)
	}
	if a.CompactThreshold() != 0 {
		t.Fatalf("CompactThreshold = %d", a.CompactThreshold())
	}
	if a.CurrentModel() == nil {
		t.Fatal("CurrentModel nil")
	}
	if a.CurrentModelName() == "" {
		t.Fatal("CurrentModelName empty")
	}
}

func TestCheckPermissionModes(t *testing.T) {
	newAgent := func(mode config.PermissionMode) *Agent {
		a := &Agent{mode: mode}
		a.toolByName = map[string]coretools.Tool{}
		return a
	}
	write := messages.ToolCall{Name: "Write", Args: map[string]any{"file_path": "x", "content": "y"}}
	read := messages.ToolCall{Name: "Read", Args: map[string]any{"file_path": "x"}}

	// Plan / read-only allow reads, refuse writes.
	plan := newAgent(config.ModePlan)
	if ok, err := plan.checkPermission(context.Background(), read); err != nil || !ok {
		t.Fatalf("plan read = %v/%v", ok, err)
	}
	if ok, _ := plan.checkPermission(context.Background(), write); ok {
		t.Fatal("plan must refuse writes")
	}
	ro := newAgent(config.ModeReadOnly)
	if ok, _ := ro.checkPermission(context.Background(), write); ok {
		t.Fatal("read-only must refuse writes")
	}

	// Accept-edits allows writes and reads; a shell tool asks (the interrupt
	// errors outside a graph run).
	ae := newAgent(config.ModeAcceptEdits)
	if ok, err := ae.checkPermission(context.Background(), write); err != nil || !ok {
		t.Fatalf("acceptEdits write = %v/%v", ok, err)
	}
	if ok, err := ae.checkPermission(context.Background(), read); err != nil || !ok {
		t.Fatalf("acceptEdits read = %v/%v", ok, err)
	}
	askOutsideRun(t, func() { _, _ = ae.checkPermission(context.Background(), messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "make"}}) })

	// Default asks for writes.
	def := newAgent(config.ModeDefault)
	askOutsideRun(t, func() { _, _ = def.checkPermission(context.Background(), write) })

	// Explicit rules win over the mode.
	rules := newAgent(config.ModeDefault)
	rules.perms = &permissions.Rules{Allow: []string{"Write"}, Deny: []string{"Read"}}
	if ok, err := rules.checkPermission(context.Background(), write); err != nil || !ok {
		t.Fatalf("allow rule = %v/%v", ok, err)
	}
	if ok, err := rules.checkPermission(context.Background(), read); err != nil || ok {
		t.Fatalf("deny rule = %v/%v", ok, err)
	}
}

func TestSpeculativeAllowedMirrorsGate(t *testing.T) {
	newAgent := func(mode config.PermissionMode) *Agent {
		return &Agent{mode: mode}
	}
	gates := []messages.ToolCall{
		{Name: "AskUserQuestion"}, {Name: "EnterPlanMode"}, {Name: "ExitPlanMode"},
	}
	for _, call := range gates {
		if newAgent(config.ModeBypass).speculativeAllowed(call) {
			t.Fatalf("%s must never pre-run", call.Name)
		}
	}
	write := messages.ToolCall{Name: "Write", Args: map[string]any{"file_path": "x"}}
	if newAgent(config.ModeBypass).speculativeAllowed(write) {
		t.Fatal("bypass must not pre-run writes (read-only speculative gate)")
	}
	if newAgent(config.ModeDefault).speculativeAllowed(write) {
		t.Fatal("default must not pre-run writes")
	}
	ask := newAgent(config.ModeBypass)
	ask.perms = &permissions.Rules{Ask: []string{"Write"}}
	if ask.speculativeAllowed(write) {
		t.Fatal("ask rule must not pre-run")
	}
}

func TestExecuteUnknownToolAndBashPolicy(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	res := a.execute(context.Background(), "NoSuchTool", nil)
	if !strings.Contains(res.Content, "unknown tool") {
		t.Fatalf("result = %q", res.Content)
	}
	// A tool returning an error becomes an Error: result.
	bad := a.toolByName["Read"]
	_ = bad
	res = a.execute(context.Background(), "Read", map[string]any{"file_path": 42})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("read error result = %q", res.Content)
	}
}

func TestGateInteractionTools(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))

	// EnterPlanMode flips plan mode and overrides the result.
	run, override, err := a.gate(context.Background(), messages.ToolCall{Name: "EnterPlanMode"})
	if run || err != nil || override == nil {
		t.Fatalf("EnterPlanMode gate = %v/%v/%v", run, override, err)
	}
	if !a.PlanMode() {
		t.Fatal("EnterPlanMode did not enable plan mode")
	}

	// ExitPlanMode without plan mode reports it.
	a.SetPlanMode(false)
	_, override, err = a.gate(context.Background(), messages.ToolCall{Name: "ExitPlanMode"})
	if err != nil || override == nil || !strings.Contains(override.Content, "Not currently in plan mode") {
		t.Fatalf("ExitPlanMode gate = %v/%v", override, err)
	}

	// ExitPlanMode with a bad args payload errors on decode.
	a.SetPlanMode(true)
	if _, _, err := a.gate(context.Background(), messages.ToolCall{Name: "ExitPlanMode", Args: map[string]any{"plan": 42}}); err == nil {
		// plan is a string field; an int decode failure surfaces the error.
		t.Fatalf("bad ExitPlanMode args must error: %v", err)
	}

	// AskUserQuestion with bad args errors.
	if _, _, err := a.gate(context.Background(), messages.ToolCall{Name: "AskUserQuestion", Args: map[string]any{"questions": 42}}); err == nil {
		t.Fatal("bad AskUserQuestion args must error")
	}

	// A denied permission yields the override result.
	denied := &Agent{mode: config.ModeDefault}
	denied.perms = &permissions.Rules{Deny: []string{"Write"}}
	run, override, err = denied.gate(context.Background(), messages.ToolCall{Name: "Write", Args: map[string]any{"file_path": "x"}})
	if run || err != nil || override == nil || !strings.Contains(override.Content, "permission denied") {
		t.Fatalf("denied gate = %v/%v/%v", run, override, err)
	}
}

func TestToolReasonAndParseAnswer(t *testing.T) {
	cases := []struct {
		call messages.ToolCall
		want string
	}{
		{messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "rm -rf /"}}, "destructive"},
		{messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "ls"}}, "Run command: ls"},
		{messages.ToolCall{Name: "Bash"}, "Bash"},
		{messages.ToolCall{Name: "Write", Args: map[string]any{"file_path": "a.txt"}}, "Write a.txt"},
		{messages.ToolCall{Name: "Edit"}, "Edit"},
		{messages.ToolCall{Name: "remember_fact", Args: map[string]any{"content": "prefers tabs"}}, "remember_fact: prefers tabs"},
		{messages.ToolCall{Name: "remember_fact"}, "remember_fact"},
		{messages.ToolCall{Name: "index_code", Args: map[string]any{"root": "/src"}}, "Index source into memory DB: /src"},
		{messages.ToolCall{Name: "index_code"}, "(working directory)"},
		{messages.ToolCall{Name: "Grep"}, "Grep"},
	}
	for _, tc := range cases {
		if got := toolReason(tc.call); !strings.Contains(got, tc.want) {
			t.Errorf("toolReason(%s) = %q, want substring %q", tc.call.Name, got, tc.want)
		}
	}

	truthy := []any{"y", "Y", "yes", " allow ", "APPROVE", "a", "true", "1", true}
	for _, v := range truthy {
		if !parseAnswer(v) {
			t.Errorf("parseAnswer(%v) = false", v)
		}
	}
	falsy := []any{"n", "no", "", "maybe", 0, nil, 3.5}
	for _, v := range falsy {
		if parseAnswer(v) {
			t.Errorf("parseAnswer(%v) = true", v)
		}
	}
}

func TestFormatAskUserAnswersAndDecodeArgs(t *testing.T) {
	got := formatAskUserAnswers([]tools.Question{{Question: "q1"}, {Question: "q2"}}, []string{"a1"})
	if !strings.Contains(got, "Q: q1\nA: a1") || !strings.Contains(got, "Q: q2\nA: (no answer)") {
		t.Fatalf("formatAskUserAnswers = %q", got)
	}
	var out struct{ X int }
	if err := decodeArgs(map[string]any{"x": 42}, &out); err != nil || out.X != 42 {
		t.Fatalf("decodeArgs = %d, %v", out.X, err)
	}
	if err := decodeArgs(map[string]any{"x": "not-an-int"}, &out); err == nil {
		t.Fatal("decodeArgs must fail on type mismatch")
	}
}

func TestFilterAndAllowTools(t *testing.T) {
	list := []coretools.Tool{mustFromFunc("A"), mustFromFunc("B"), mustFromFunc("C")}
	if got := filterTools(list, "B"); len(got) != 2 || got[0].Name() != "A" || got[1].Name() != "C" {
		t.Fatalf("filterTools = %v", got)
	}
	if got := allowTools(list, []string{"A", "C", "Z"}); len(got) != 2 || got[1].Name() != "C" {
		t.Fatalf("allowTools = %v", got)
	}
}

func TestForceCompactWithoutSummarizer(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))))
	history := []messages.Message{messages.Human("x")}
	if got := a.ForceCompact(context.Background(), history); len(got) != 1 {
		t.Fatalf("ForceCompact without summarizer = %d msgs", len(got))
	}
}

func TestPreCompactHookBlocksCompaction(t *testing.T) {
	runner := hooks.NewRunner(hooks.Config{
		PreCompact: []hooks.Entry{{Hooks: []hooks.Hook{{Command: "block"}}}},
	}, ".", "s1", hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		return hooks.CommandResult{ExitCode: 2, Stdout: "not now"}, nil // exit 2 = blocked
	}))
	a, err := New(Config{
		Model:            language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:         tools.NewRegistry(t.TempDir()),
		SystemPrompt:     "test",
		PermissionMode:   config.ModeBypass,
		CompactThreshold: 10,
		Summarizer:       language.NewFakeChatModel(language.WithResponses(messages.AI("sum"))),
		Hooks:            runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	history := bigHistory()
	if got := a.CompactHistory(context.Background(), history); len(got) != len(history) {
		t.Fatalf("blocked compaction changed history: %d -> %d", len(history), len(got))
	}
}

func TestStreamDelegatesAndResumeCheckpointError(t *testing.T) {
	a := newBypassAgent(t, language.NewFakeChatModel(language.WithResponses(messages.AI("hi"))))
	for _, err := range a.Stream(context.Background(), []messages.Message{messages.Human("x")}, "t") {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
	}
	// ResumeCheckpoint on an unknown thread errors.
	if _, err := a.ResumeCheckpoint(context.Background(), "no-such-thread", "y"); err == nil {
		t.Fatal("ResumeCheckpoint on unknown thread must error")
	}
}

// namedModel exposes a Name() so CurrentModelName picks it up.
type namedModel2 struct {
	*language.FakeChatModel
}

func (namedModel2) Name() string { return "fake-1" }

func TestCurrentModelNameUsesNameMethod(t *testing.T) {
	a, err := New(Config{
		Model:          namedModel2{language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))},
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.CurrentModelName(); got != "fake-1" {
		t.Fatalf("CurrentModelName = %q", got)
	}
}

func TestRunTurnSurfacesGraphError(t *testing.T) {
	a := newBypassAgent(t, erroringSubagentModel{language.NewFakeChatModel()})
	if _, err := a.runTurn(context.Background(), []messages.Message{messages.Human("x")}, "t"); err == nil {
		t.Fatal("model error must propagate through the graph")
	}
}

// askOutsideRun asserts that a permission ask outside a graph run does not
// silently allow: the fork runtime's Interrupt guard panics (there is no
// node execution to pause), which this helper recovers as the expected
// outcome. It fails the test when the call instead returns cleanly.
func askOutsideRun(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("ask outside a graph run must not return cleanly (interrupt guard expected)")
		}
	}()
	f()
}
