package agent

// Task 5 (graph-native HITL): the hooks and approval supersteps. Gates,
// guards and PreToolUse hooks run in a dedicated `hooks` node that never
// interrupts; every user ask is collected by the `approval` node into ONE
// ApprovalRequest interrupt whose ApprovalAnswer persists the decisions in
// graph state. These tests revive the archived relay-idempotency repros
// R4/R5/N2a/N2b against the new topology (archive:
// woca-internal-docs/relay-idempotency-repro-2026-11-08_test.go.txt) plus
// the new envelope, grant-key and legacy-checkpoint pins. Shared fixtures
// (memoProbeTool, pumpStream, transcriptModel, writeCall, logToolMsgs) live
// in taskrelay_test.go and relayidem_helpers_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/cpserde"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

// collectPauses drives a stream to completion (or the given number of
// pauses), answering each pause through answer, and returns the decoded
// pause payloads in order plus the final transcript.
func collectPauses(t *testing.T, a *Agent, ctx context.Context, thread string, answer func(t *testing.T, i int, decoded any) any) ([]any, []messages.Message) {
	t.Helper()
	stream := a.Stream(ctx, []messages.Message{messages.Human("go")}, thread)
	var pauses []any
	var final []messages.Message
	for i := 0; ; i++ {
		intrs, vals := pumpStream(t, stream)
		if len(vals) > 0 {
			final = vals
		}
		if len(intrs) == 0 {
			return pauses, final
		}
		if i > 5 {
			t.Fatalf("too many pauses (last: %v)", decodeInterruptPayload(intrs[0].Value))
		}
		decoded := DecodeInterruptValue(intrs[0].Value)
		pauses = append(pauses, decoded)
		if answer == nil {
			t.Fatalf("unexpected pause #%d: %T", i+1, decoded)
		}
		stream = a.ResumeStream(ctx, thread, answer(t, i, decoded))
	}
}

// R4 — revived: PreToolUse hooks fire exactly once per executed call. The
// hooks node never interrupts, so the Write ask pausing the graph can no
// longer re-run memo_probe's gate+hook loop. Batch [memo_probe (allow-ruled),
// Write (asks)]; want exactly 1 hook fire and 1 execution across the pause.
func TestReproR4PreToolUseHookRefiresOnResume(t *testing.T) {
	workdir := t.TempDir()
	var hookFires, n atomic.Int32
	hr := hooks.NewRunner(
		hooks.Config{PreToolUse: []hooks.Entry{{Matcher: "memo_probe", Hooks: []hooks.Hook{{Command: "audit"}}}}},
		workdir, "s1",
		hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			hookFires.Add(1)
			return hooks.CommandResult{ExitCode: 0}, nil
		}),
	)
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "memo_probe", Args: map[string]any{}},
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "hooked.txt", "content": "x"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		ExtraTools:     []coretools.Tool{memoProbeTool(&n)},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"memo_probe"}},
		Hooks:          hr,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-repro-r4"
	pauses, _ := collectPauses(t, a, ctx, thread, func(t *testing.T, i int, decoded any) any {
		req, ok := decoded.(ApprovalRequest)
		if !ok {
			t.Fatalf("pause payload = %T, want ApprovalRequest", decoded)
		}
		if len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
			t.Fatalf("asks = %+v, want one Write ask", req.Asks)
		}
		return ApprovalAnswer{Decisions: map[string]AskDecision{
			req.Asks[0].CallID: {Outcome: OutcomeAllow},
		}}
	})
	if len(pauses) != 1 {
		t.Fatalf("pauses = %d, want 1", len(pauses))
	}
	if got := n.Load(); got != 1 {
		t.Errorf("memo_probe executed %d times, want 1", got)
	}
	if got := hookFires.Load(); got != 1 {
		t.Errorf("HOOK RE-FIRE: PreToolUse hook for memo_probe fired %d times for one execution, want 1", got)
	}
}

// R5 — revived: the Task relay's EXECUTION-phase pause (the tools node
// re-executes on every parent resume) must not re-fire the Task call's
// PreToolUse hook; the hook ran once in the hooks superstep and the re-entry
// skips it (decisions persist in graph state). One Task call relaying two
// sequential child permission pauses → want exactly 1 hook fire.
func TestReproR5ExecPhasePauseHookRefires(t *testing.T) {
	workdir := t.TempDir()
	var hookFires atomic.Int32
	hr := hooks.NewRunner(
		hooks.Config{PreToolUse: []hooks.Entry{{Matcher: "Task", Hooks: []hooks.Hook{{Command: "audit"}}}}},
		workdir, "s1",
		hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			hookFires.Add(1)
			return hooks.CommandResult{ExitCode: 0}, nil
		}),
	)
	parentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Task", Args: map[string]any{
					"description": "write files",
					"prompt":      "write the files",
				}},
			}},
			messages.AI("parent done"),
		),
	)
	a, err := New(Config{
		Model: parentModel,
		SubagentModel: &transcriptModel{script: []messages.Message{
			writeCall("w1", "a.txt", "a"),
			writeCall("w2", "b.txt", "b"),
			messages.AI("sub done"),
		}},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
		Hooks:          hr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-repro-r5"
	stream := a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread)
	pauses := 0
	for {
		intrs, _ := pumpStream(t, stream)
		if len(intrs) == 0 {
			break
		}
		pauses++
		if pauses > 2 {
			t.Fatalf("unexpected extra pause")
		}
		stream = a.ResumeStream(ctx, thread, "y")
	}
	if pauses != 2 {
		t.Fatalf("relay pauses = %d, want 2 (one per child Write)", pauses)
	}
	if got := hookFires.Load(); got != 1 {
		t.Errorf("HOOK RE-FIRE (exec-phase): PreToolUse hook for the single Task call fired %d times, want 1", got)
	}
}

// N2a — revived/adapted: the archived defect needed the sequential gate to
// re-derive mid-batch (plan approval flipped planMode off, then the Bash ask
// consumed the plan's "y"). In the new topology the WHOLE batch evaluates in
// one hooks pass: under plan=true the Bash is denied outright (never asked),
// so only the ExitPlanMode ask exists and no answer can shift onto Bash.
// Approving the plan flips the flag; the Bash NEVER executes this round.
func TestReproN2aPlanApprovalAnswerShiftsOntoBash(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "e1", Name: "ExitPlanMode", Args: map[string]any{"plan": "the plan"}},
				{ID: "b1", Name: "Bash", Args: map[string]any{"command": "touch n2a.txt"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.SetPlanMode(true)
	ctx := t.Context()
	thread := "thread-repro-n2a"
	pauses, final := collectPauses(t, a, ctx, thread, func(t *testing.T, i int, decoded any) any {
		req, ok := decoded.(ApprovalRequest)
		if !ok {
			t.Fatalf("pause payload = %T, want ApprovalRequest", decoded)
		}
		if len(req.Asks) != 1 || req.Asks[0].ToolName != "ExitPlanMode" {
			t.Fatalf("asks = %+v, want ONLY the plan ask (the in-plan Bash is denied outright, never asked)", req.Asks)
		}
		return ApprovalAnswer{Decisions: map[string]AskDecision{
			"e1": {Outcome: OutcomeAllow},
			"b1": {Outcome: OutcomeDeny},
		}}
	})
	if len(pauses) != 1 {
		t.Fatalf("pauses = %d, want exactly 1 (one envelope; the plan answer can never shift onto Bash)", len(pauses))
	}
	if a.PlanMode() {
		t.Error("plan flag must flip off after the approved ExitPlanMode")
	}
	logToolMsgs(t, "final", final)
	if _, statErr := os.Stat(filepath.Join(workdir, "n2a.txt")); statErr == nil {
		t.Errorf("CONSENT MISALIGNMENT: Bash executed although the user denied it in the same envelope")
	}
	if !toolContentContains(final, "permission denied") {
		t.Error("the denied Bash produced no denial marker in the transcript")
	}
}

// N2b — revived/adapted: with decisions committed to graph state at
// gate-evaluation time, a permission-mode switch WHILE PARKED cannot change
// an asked call's outcome. Batch [Write a, Bash touch] in default mode; while
// the single envelope is parked the user flips to acceptEdits (which would
// auto-allow the Write on re-derivation), then answers Write=allow, Bash=deny.
// Want: a.txt exists, n2b.txt does not — the Bash "n" lands on Bash.
func TestReproN2bModeSwitchShiftsAnswers(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "a.txt", "content": "a"}},
				{ID: "b1", Name: "Bash", Args: map[string]any{"command": "touch n2b.txt"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-repro-n2b"
	stream := a.Stream(ctx, []messages.Message{messages.Human("go")}, thread)
	intrs, _ := pumpStream(t, stream)
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1 (one envelope for both asks)", len(intrs))
	}
	req, ok := DecodeInterruptValue(intrs[0].Value).(ApprovalRequest)
	if !ok {
		t.Fatalf("pause payload = %T, want ApprovalRequest", DecodeInterruptValue(intrs[0].Value))
	}
	if len(req.Asks) != 2 {
		t.Fatalf("asks = %+v, want the Write ask AND the Bash ask", req.Asks)
	}
	// The user flips the mode while parked — under re-derivation the Write
	// would stop asking; the committed decisions must be immune.
	a.SetMode(config.ModeAcceptEdits)
	_, final := pumpStream(t, a.ResumeStream(ctx, thread, ApprovalAnswer{Decisions: map[string]AskDecision{
		"w1": {Outcome: OutcomeAllow},
		"b1": {Outcome: OutcomeDeny},
	}}))
	if _, err := os.Stat(filepath.Join(workdir, "a.txt")); err != nil {
		t.Errorf("approved Write a.txt did not run: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, "n2b.txt")); statErr == nil {
		t.Errorf("CONSENT MISALIGNMENT: Bash executed although the user answered deny (mode switch while parked shifted the answer)")
	}
	logToolMsgs(t, "final", final)
}

// TestAskEnvelopeCarriesGrantKey: the ask envelope must precompute the
// session-grant key (GrantKey) so an "allow for session" answer can bind to
// the rule it creates.
func TestAskEnvelopeCarriesGrantKey(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "g1", Name: "Bash", Args: map[string]any{"command": "git push --force origin main"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-grantkey-envelope"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("push")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1", len(intrs))
	}
	req, ok := DecodeInterruptValue(intrs[0].Value).(ApprovalRequest)
	if !ok {
		t.Fatalf("pause payload = %T, want ApprovalRequest", DecodeInterruptValue(intrs[0].Value))
	}
	if len(req.Asks) != 1 {
		t.Fatalf("asks = %+v, want one", req.Asks)
	}
	ask := req.Asks[0]
	if ask.CallID != "g1" || ask.ToolName != "Bash" {
		t.Fatalf("ask = %+v, want the g1 Bash call", ask)
	}
	if ask.SessionRuleKey != "bash:git push*" {
		t.Fatalf("SessionRuleKey = %q, want bash:git push*", ask.SessionRuleKey)
	}
	if ask.Origin != "main" {
		t.Fatalf("Origin = %q, want main", ask.Origin)
	}
	// Answer "allow for session" through the envelope: the grant mutator
	// must record it, and an identical second call runs without asking.
	stream := a.ResumeStream(ctx, thread, ApprovalAnswer{Decisions: map[string]AskDecision{
		"g1": {Outcome: OutcomeAllow, Grant: GrantSession},
	}})
	pauses, final := 0, []messages.Message(nil)
	for {
		intrs2, vals := pumpStream(t, stream)
		if len(vals) > 0 {
			final = vals
		}
		if len(intrs2) == 0 {
			break
		}
		pauses++
		if pauses > 2 {
			t.Fatalf("unexpected pause")
		}
		stream = a.ResumeStream(ctx, thread, ApprovalAnswer{Decisions: map[string]AskDecision{
			"g1": {Outcome: OutcomeAllow, Grant: GrantSession},
		}})
	}
	if pauses != 0 {
		t.Fatalf("second identical granted call paused %d time(s), want 0 (session grant applied)", pauses)
	}
	if got := final[len(final)-1].Content; got != "done" {
		t.Fatalf("final message = %q, want done (final transcript: %v)", got, final)
	}
	if got := a.PolicySource().ListGrants(""); len(got) != 1 || got[0] != "bash:git push*" {
		t.Fatalf("ListGrants = %v, want [bash:git push*]", got)
	}
}

// TestApprovalNodeResumesOnce: a batch with several asks surfaces exactly
// ONE pause; one ApprovalAnswer answers all of them and the run completes
// without further pauses.
func TestApprovalNodeResumesOnce(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "one.txt", "content": "1"}},
				{ID: "w2", Name: "Write", Args: map[string]any{"file_path": "two.txt", "content": "2"}},
				{ID: "b1", Name: "Bash", Args: map[string]any{"command": "touch three.txt"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-approval-once"
	pauses, final := collectPauses(t, a, ctx, thread, func(t *testing.T, i int, decoded any) any {
		req, ok := decoded.(ApprovalRequest)
		if !ok {
			t.Fatalf("pause payload = %T, want ApprovalRequest", decoded)
		}
		if len(req.Asks) != 3 {
			t.Fatalf("asks = %+v, want all three in one envelope", req.Asks)
		}
		return ApprovalAnswer{Decisions: map[string]AskDecision{
			"w1": {Outcome: OutcomeAllow},
			"w2": {Outcome: OutcomeDeny},
			"b1": {Outcome: OutcomeAllow, Grant: GrantSession},
		}}
	})
	if len(pauses) != 1 {
		t.Fatalf("pauses = %d, want exactly 1 for a 3-ask batch", len(pauses))
	}
	if _, err := os.Stat(filepath.Join(workdir, "one.txt")); err != nil {
		t.Error("allowed Write one.txt did not run")
	}
	if _, err := os.Stat(filepath.Join(workdir, "two.txt")); err == nil {
		t.Error("denied Write two.txt executed anyway")
	}
	if _, err := os.Stat(filepath.Join(workdir, "three.txt")); err != nil {
		t.Error("allowed Bash touch did not run")
	}
	if got := final[len(final)-1].Content; got != "done" {
		t.Fatalf("run did not complete; final: %v", final)
	}
}

// legacyCheckpointAgent runs a MINIMAL old-topology graph (model → tools,
// tools raising the legacy in-node gate interrupt) over a durable sqlite
// saver until it pauses — hand-crafting a relay-era checkpoint whose pending
// interrupt lives on the `tools` node. The node names match the agent graph
// ("model", "tools") and the state carries only the messages channel (the
// decisions channel did not exist), which is exactly the legacy shape.
func legacyCheckpointAgent(t *testing.T, dbPath, thread string, calls ...messages.ToolCall) {
	t.Helper()
	saver, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = saver.Close() }()
	g := lggraph.NewStateGraph()
	g.AddReducer("messages", messagesChannel)
	g.AddNode("model", func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		return nil, nil
	})
	g.AddNode("tools", func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		// The pre-Task-5 tools node interrupted INSIDE the node: the gate's
		// permission ask, pending on the tools task.
		lggraph.Interrupt(rt, encodeInterruptPayload(PermissionRequest{ToolName: "Bash", Reason: "legacy"}))
		return nil, nil
	})
	g.AddEdge("tools", "model")
	g.AddConditionalEdges("model", func(rt lgruntime.Runtime, state map[string]any) ([]any, error) {
		msgs := MessagesFrom(state)
		if len(msgs) == 0 {
			return []any{lgtypes.END}, nil
		}
		last := msgs[len(msgs)-1]
		if last.Role == messages.RoleAI && len(last.ToolCalls) > 0 {
			return []any{"tools"}, nil
		}
		return []any{lgtypes.END}, nil
	})
	g.SetEntryPoint("model")
	cg, err := g.Compile(lggraph.WithCheckpointer(saver))
	if err != nil {
		t.Fatalf("compile legacy graph: %v", err)
	}
	msgs := []messages.Message{
		messages.Human("run it"),
		{Role: messages.RoleAI, ToolCalls: calls},
	}
	res, err := cg.InvokeWithOptions(t.Context(), map[string]any{"messages": msgs}, lggraph.Options{ThreadID: thread})
	if err != nil {
		t.Fatalf("legacy graph invoke: %v", err)
	}
	if len(res.Interrupts) == 0 {
		t.Fatal("legacy graph should have paused on the tools interrupt")
	}
}

// TestLegacyToolsPauseMarkedInterrupted (audit C1, moved from Task 9): a
// relay-era checkpoint (pending interrupt addressed to the tools node, no
// decisions channel in state) must NEVER resume into ungated execution. The
// new tools node detects the missing decisions and synthesizes interrupted
// marker results for the whole batch.
func TestLegacyToolsPauseMarkedInterrupted(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacyCheckpointAgent(t, dbPath, "legacy-tools-pause",
		messages.ToolCall{ID: "b1", Name: "Bash", Args: map[string]any{"command": "touch " + filepath.Join(workdir, "leaked.txt")}},
	)

	saver, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = saver.Close() }()
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("understood, stopping")))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
		Checkpointer:   saver,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The resumed thread runs under bypass mode: without the guard the
	// legacy batch would execute ungated (the touch below would land).
	final, err := a.Resume(t.Context(), "legacy-tools-pause", "y")
	if err != nil {
		t.Fatalf("legacy resume: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, "leaked.txt")); statErr == nil {
		t.Fatal("LEGACY UNGATED EXECUTION: side effect landed despite the legacy guard")
	}
	if !toolContentContains(final, "interrupted (pre-upgrade checkpoint)") {
		t.Fatalf("final transcript lacks the interrupted marker: %v", final)
	}
}
