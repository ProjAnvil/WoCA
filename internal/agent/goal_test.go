package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newGoalAgent builds a bypass-permission agent over a scripted fake model.
func newGoalAgent(t *testing.T, responses ...messages.Message) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(responses...),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// goalPumpResult is the drained terminal state of a goal chunk stream.
type goalPumpResult struct {
	values     map[string]any
	interrupts []lgtypes.Interrupt // non-empty when the loop paused for an answer
	err        error               // terminal error (nil on completion or pause)
}

// pumpGoalChunks drains a goal chunk stream to its terminal state (completion,
// pause, or error) and reports what it ended with.
func pumpGoalChunks(t *testing.T, s iter.Seq2[lggraph.StreamChunk, error]) goalPumpResult {
	t.Helper()
	var out goalPumpResult
	for c, cerr := range s {
		if cerr != nil {
			out.err = cerr
			return out
		}
		if intrs := goalInterrupts(c.Mode, c.Payload); len(intrs) > 0 {
			for i := range intrs {
				intrs[i].Value = decodeInterruptPayload(intrs[i].Value)
			}
			out.interrupts = intrs
		}
		if c.Mode == lggraph.StreamValues {
			out.values = valuesState(c.Payload)
		}
	}
	return out
}

func TestGoalCompletesAfterVerification(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("done-criteria checklist\nThis cycle: implement the feature"),           // assess
		messages.AI("implemented and verified"),                                             // execute
		messages.AI(`{"verdict":"complete","confidence":0.9,"evidence":"tests pass: 3/3"}`), // evaluate
	)
	if _, err := a.SetGoal("make the tests pass", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	stream, err := a.GoalStream(context.Background(), []messages.Message{messages.Human("hello")})
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("goal stream ended with error: %v", resg.err)
	}
	g := a.Goal()
	if g.Status != GoalComplete {
		t.Fatalf("status = %q, want complete", g.Status)
	}
	if g.Evidence != "tests pass: 3/3" {
		t.Fatalf("evidence = %q", g.Evidence)
	}
	if len(g.Progress) != 1 || g.Progress[0].Verdict != "complete" {
		t.Fatalf("progress = %+v", g.Progress)
	}
	msgs := MessagesFrom(resg.values)
	found := false
	for _, m := range msgs {
		if m.Role == messages.RoleAI && strings.Contains(m.Content, "Goal complete") {
			found = true
		}
	}
	if !found {
		t.Fatal("final transcript misses the completion report")
	}
	// The transcript keeps the cycle prompt and the executor's work.
	if !strings.Contains(msgs[len(msgs)-2].Content, "implemented and verified") {
		t.Fatalf("cycle messages missing from final state: %+v", msgs[len(msgs)-3:])
	}
	if a.GoalRunning() {
		t.Fatal("goal run flag not cleared")
	}
}

func TestGoalCycleSurvivesInnerRecursionLimit(t *testing.T) {
	// A goal execute cycle that outgrows the inner agent graph's recursion
	// limit must not kill the goal loop: the cycle's work is salvaged,
	// dangling tool calls are answered, and evaluation proceeds. (Regression:
	// "graph: recursion limit 50 exceeded at node \"model\"" used to end the
	// run and drop every message the cycle had produced.)
	tc := func(id string) messages.Message {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: id, Name: "Read", Args: map[string]any{"file_path": "x"},
		}}}
	}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("assess 1"), // cycle 1 assess
			// cycle 1 execute: with the doubled cycle budget (2×5=10
			// supersteps) five tool rounds complete; the sixth model
			// dispatch is refused and the cycle is salvaged.
			tc("1"), tc("2"), tc("3"), tc("4"), tc("5"),
			messages.AI(`{"verdict":"continue","gaps":"more to do"}`), // cycle 1 evaluate
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 5, // turn budget 5 → goal-cycle budget 10
	})
	if err != nil {
		t.Fatal(err)
	}
	a.SetGoal("long-running goal", "", 2, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("goal loop died on the recursion limit instead of salvaging: %v", resg.err)
	}

	// Both cycles ran to their evaluations; the loop ended on its own budget.
	g := a.Goal()
	if g.Status != GoalBudgetLimited {
		t.Fatalf("status = %q, want budget_limited after 2 salvaged cycles", g.Status)
	}
	if len(g.Progress) != 2 {
		t.Fatalf("progress entries = %d, want 2 (both cycles were evaluated)", len(g.Progress))
	}
	if a.GoalRunning() {
		t.Fatal("goal run flag not cleared")
	}

	msgs := MessagesFrom(resg.values)
	markers := 0
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == messages.RoleAI && strings.Contains(m.Content, "goal cycle cut short") {
			markers++
		}
		if m.Role == messages.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	if markers == 0 {
		t.Fatal("salvaged transcript misses the cut-short marker")
	}
	// Every tool call in the salvaged transcript must have a tool result, or
	// the next cycle would feed an invalid history to the provider.
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			if !answered[call.ID] {
				t.Fatalf("tool call %q has no result: transcript would be rejected by the provider", call.ID)
			}
		}
	}
}

// TestGoalCycleStepBudgetAndLimitDetection pins the cycle-budget formula and
// the typed detection of the runtime's recursion-limit error.
func TestGoalCycleStepBudgetAndLimitDetection(t *testing.T) {
	cases := []struct{ turnLimit, want int }{
		{0, 100},   // unset → default turn limit 50, doubled
		{-3, 100},  // negative → same as unset
		{5, 10},    // the integration test's configuration
		{50, 100},  // the default configuration
		{100, 200}, // scales with a user-raised turn limit
	}
	for _, tc := range cases {
		if got := goalCycleStepBudget(tc.turnLimit); got != tc.want {
			t.Errorf("goalCycleStepBudget(%d) = %d, want %d", tc.turnLimit, got, tc.want)
		}
	}

	typed := &lgtypes.GraphRecursionError{Limit: 10, Node: "model"}
	if !isRecursionLimit(typed) {
		t.Fatal("typed GraphRecursionError not detected")
	}
	if !isRecursionLimit(fmt.Errorf("wrapped: %w", typed)) {
		t.Fatal("wrapped GraphRecursionError not detected")
	}
	if isRecursionLimit(errors.New("graph: recursion limit (10) exceeded")) {
		t.Fatal("a lookalike plain error must not be detected — detection is typed, not string-based")
	}
	if isRecursionLimit(nil) || isRecursionLimit(errors.New("boom")) {
		t.Fatal("unrelated errors must not be detected")
	}
}

// TestAnswerDanglingToolCalls covers the salvage path where the cut lands
// between the model and tools nodes: the trailing unanswered tool-call
// message gets synthetic results so the transcript stays provider-valid.
func TestAnswerDanglingToolCalls(t *testing.T) {
	call := func(id string) messages.ToolCall {
		return messages.ToolCall{ID: id, Name: "Read", Args: map[string]any{"file_path": "x"}}
	}
	dangling := []messages.Message{
		messages.Human("<goal-cycle 1>"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{call("1"), call("2")}},
	}
	out := answerDanglingToolCalls(dangling)
	if len(out) != len(dangling)+2 {
		t.Fatalf("len = %d, want synthetic results for both calls", len(out))
	}
	for i, want := range []string{"1", "2"} {
		m := out[len(dangling)+i]
		if m.Role != messages.RoleTool || m.ToolCallID != want {
			t.Fatalf("synthetic result %d = %+v, want tool result for call %q", i, m, want)
		}
		if !strings.Contains(m.Content, "never ran") {
			t.Fatalf("synthetic result %d content = %q", i, m.Content)
		}
	}
	// No dangling tail: the input is returned as-is (no copies, no additions).
	clean := []messages.Message{messages.Human("hi"), messages.AI("done")}
	if got := answerDanglingToolCalls(clean); len(got) != 2 {
		t.Fatalf("clean input altered: %+v", got)
	}
	if got := answerDanglingToolCalls(nil); got != nil {
		t.Fatalf("nil input altered: %+v", got)
	}
	// Already-answered tail: nothing to add.
	answered := append(append([]messages.Message(nil), dangling...), messages.Tool("1", "ok"), messages.Tool("2", "ok"))
	if got := answerDanglingToolCalls(answered); len(got) != len(answered) {
		t.Fatalf("answered input altered: %+v", got)
	}
}

// TestStreamWithBudgetAppliesRecursionLimit proves the per-invocation budget
// actually reaches the runtime: a looping model trips the limit and the typed
// error reports the OVERRIDDEN limit, not the compiled one.
func TestStreamWithBudgetAppliesRecursionLimit(t *testing.T) {
	loop := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
		ID: "1", Name: "Read", Args: map[string]any{"file_path": "x"},
	}}}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(loop), // every model call requests another tool round
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 50, // compiled default the override must beat
	})
	if err != nil {
		t.Fatal(err)
	}
	var hit *lgtypes.GraphRecursionError
	for _, err := range a.streamWithBudget(context.Background(), nil, "budget-test", 7) {
		if err == nil {
			continue
		}
		if !errors.As(err, &hit) {
			t.Fatalf("unexpected stream error: %v", err)
		}
		break
	}
	if hit == nil {
		t.Fatal("stream ended without tripping the recursion limit")
	}
	if hit.Limit != 7 {
		t.Fatalf("error reports limit %d, want the overridden 7 (compiled is 50)", hit.Limit)
	}
}

func TestGoalBudgetLimitedByCycles(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.AI("work 1"),
		messages.AI(`{"verdict":"continue","gaps":"not done"}`))
	a.SetGoal("endless goal", "", 1, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", resg.err)
	}
	if g := a.Goal(); g.Status != GoalBudgetLimited {
		t.Fatalf("status = %q, want budget_limited", g.Status)
	}
}

func TestGoalStallBlockedAfterTwoEmptyCycles(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.AI("thinking only, no tools"), // zero tool rounds
		messages.AI(`{"verdict":"continue","gaps":"nothing happened"}`), messages.AI("assess 2"),
		messages.AI("still just talking"), // zero tool rounds again
		messages.AI(`{"verdict":"continue","gaps":"nothing happened"}`))
	a.SetGoal("do something", "", 10, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", resg.err)
	}
	g := a.Goal()
	if g.Status != GoalBlocked {
		t.Fatalf("status = %q, want blocked (empty-cycle suppression)", g.Status)
	}
	if !strings.Contains(g.BlockedReason, "no observable progress") {
		t.Fatalf("blocked reason = %q", g.BlockedReason)
	}
}

func TestGoalClaimNeedsEvaluatorVerification(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "1", Name: "UpdateGoal",
			Args: map[string]any{"action": "claim_complete", "evidence": "looks done to me"},
		}}},
		messages.AI("I believe it is done"),
		messages.AI(`{"verdict":"continue","gaps":"no tests were run"}`), // claim rejected
		messages.AI("assess 2"),
		messages.AI("ran the tests"),
		messages.AI(`{"verdict":"complete","evidence":"tests green"}`),
	)
	a.SetGoal("ship it", "", 5, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", resg.err)
	}
	g := a.Goal()
	if g.Status != GoalComplete {
		t.Fatalf("status = %q, want complete after re-verification", g.Status)
	}
	if g.Claimed {
		t.Fatal("unverified claim was not reset")
	}
	// The UpdateGoal tool ran inside the cycle (a tool message came back).
	found := false
	for _, m := range MessagesFrom(resg.values) {
		if m.Role == messages.RoleTool && m.ToolCallID == "1" {
			found = true
		}
	}
	if !found {
		t.Fatal("UpdateGoal tool result missing from the transcript")
	}
}

func TestGoalInterruptResumesMidCycle(t *testing.T) {
	// Default permission mode: the Write call inside the execute cycle must
	// interrupt, park at the harness, and resume mid-cycle without redoing
	// the assess stage.
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("assess 1"), // assess
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
				ID: "1", Name: "Write",
				Args: map[string]any{"file_path": "out.txt", "content": "hi"},
			}}}, // execute: asks for a write → permission interrupt
			messages.AI("wrote the file"),                                   // execute: continues after approval
			messages.AI(`{"verdict":"complete","evidence":"file written"}`), // evaluate
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.SetGoal("write a file", "", 5, 1_000_000)

	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := pumpGoalChunks(t, stream)
	if len(res.interrupts) == 0 {
		t.Fatalf("expected a permission interrupt, got err=%v", res.err)
	}
	req, ok := res.interrupts[0].Value.(PermissionRequest)
	if !ok || req.ToolName != "Write" {
		t.Fatalf("interrupt value = %#v", res.interrupts[0].Value)
	}

	resumed, err := a.GoalResume(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	res = pumpGoalChunks(t, resumed)
	if res.err != nil || len(res.interrupts) > 0 {
		t.Fatalf("resume failed: %v (interrupts: %v)", res.err, res.interrupts)
	}
	if g := a.Goal(); g.Status != GoalComplete {
		t.Fatalf("status = %q, want complete", g.Status)
	}
	if a.GoalRunning() {
		t.Fatal("goal run flag not cleared after resume-completion")
	}
	// Exactly one assess ran (the resume did not restart the cycle).
	found := 0
	for _, m := range MessagesFrom(res.values) {
		if m.Role == messages.RoleAI && m.Content == "assess 1" {
			found++
		}
	}
	if found != 0 { // assess output never enters the transcript
		t.Fatalf("assess leaked into the transcript %d times", found)
	}
	toolResult := false
	for _, m := range MessagesFrom(res.values) {
		if m.Role == messages.RoleTool && m.ToolCallID == "1" {
			toolResult = true
		}
	}
	if !toolResult {
		t.Fatal("approved write result missing from the transcript")
	}
}

func TestGoalPauseAndResume(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "1", Name: "Read", Args: map[string]any{"file_path": "x"},
		}}},
		messages.AI("work"),
		messages.AI(`{"verdict":"complete","evidence":"done"}`))
	a.SetGoal("goal", "", 5, 1_000_000)
	if !a.PauseGoal() {
		t.Fatal("pause failed")
	}
	if _, err := a.GoalStream(context.Background(), nil); err == nil {
		t.Fatal("GoalStream should refuse a paused goal")
	}
	if !a.ResumeGoal() {
		t.Fatal("resume failed")
	}
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", resg.err)
	}
	if g := a.Goal(); g.Status != GoalComplete {
		t.Fatalf("status = %q, want complete", g.Status)
	}
	if a.ResumeGoal() {
		t.Fatal("resume should refuse a completed goal")
	}
	a.ClearGoal()
	if a.Goal() != nil {
		t.Fatal("clear failed")
	}
}

func TestGoalClearEndsLoopAtBoundary(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.AI("work"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`), messages.AI("assess 2"),
		messages.AI("work 2"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`))
	a.SetGoal("goal", "", 10, 1_000_000)
	stream, _ := a.GoalStream(context.Background(), nil)
	cycles := 0
	var termErr error
	for c, cerr := range stream {
		if cerr != nil {
			termErr = cerr
			break
		}
		if c.Mode == lggraph.StreamCustom {
			if s, ok := c.Payload.(string); ok && strings.Contains(s, "[goal cycle") {
				cycles++
				if cycles == 1 {
					a.ClearGoal() // user clears mid-loop
				}
			}
		}
	}
	if termErr != nil {
		t.Fatalf("loop should end cleanly after clear, got %v", termErr)
	}
	// The clear lands asynchronously: the loop may already have emitted the
	// next cycle header before the ClearGoal call below takes effect, so the
	// deterministic assertion is that it ends cleanly and bounded — without
	// the clear, all 10 cycles would run (fake responses repeat "continue").
	if cycles > 2 {
		t.Fatalf("ran %d cycles after clear, want ≤2 (clear did not stop the loop)", cycles)
	}
}

func TestGoalToolRestrictedActions(t *testing.T) {
	a := newGoalAgent(t)
	// No goal: read-only report.
	res := a.execute(context.Background(), "GetGoal", nil)
	if res.Content != "(no active goal)" {
		t.Fatalf("GetGoal = %q", res.Content)
	}
	a.SetGoal("restricted", "", 5, 1_000_000)
	// The model cannot pause/resume/clear.
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "pause"}); !strings.Contains(res.Content, "unknown action") {
		t.Fatalf("pause should be rejected: %q", res.Content)
	}
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "clear"}); !strings.Contains(res.Content, "unknown action") {
		t.Fatalf("clear should be rejected: %q", res.Content)
	}
	// Progress, claim, blocked all work and are visible in snapshots.
	a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "progress", "note": "halfway"})
	if g := a.Goal(); len(g.Notes) != 1 || g.Notes[0] != "halfway" {
		t.Fatalf("notes = %+v", g.Notes)
	}
	a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "claim_complete", "evidence": "e"})
	if g := a.Goal(); !g.Claimed {
		t.Fatal("claim not recorded")
	}
	if g := a.Goal(); g.Status != GoalActive {
		t.Fatalf("claim must not complete the goal by itself, status = %q", g.Status)
	}
	a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "blocked", "reason": "need creds"})
	if g := a.Goal(); g.Status != GoalBlocked || g.BlockedReason != "need creds" {
		t.Fatalf("blocked not applied: %+v", g)
	}
}

func TestParseGoalCommand(t *testing.T) {
	cases := []struct {
		args    []string
		verb    string
		obj     string
		wantErr bool
	}{
		{nil, "status", "", false},
		{[]string{"set", "make", "it", "work"}, "set", "make it work", false},
		{[]string{"set", "--cycles", "3", "do", "things"}, "set", "do things", false},
		{[]string{"set"}, "", "", true},
		{[]string{"pause"}, "pause", "", false},
		{[]string{"resume"}, "resume", "", false},
		{[]string{"clear"}, "clear", "", false},
		{[]string{"budget", "cycles", "7"}, "budget", "", false},
		{[]string{"budget", "tokens", "x"}, "", "", true},
		// Free text without a verb is shorthand for "set" — the natural
		// input users type first (whole sentences, flags, or single words).
		{[]string{"测试，你能使用这个技能吗？"}, "set", "测试，你能使用这个技能吗？", false},
		{[]string{"make", "it", "work"}, "set", "make it work", false},
		{[]string{"wibble"}, "set", "wibble", false},
		{[]string{"--cycles", "3", "do", "things"}, "set", "do things", false},
		{[]string{"--cycles"}, "", "", true},
	}
	for _, tc := range cases {
		cmd, err := ParseGoalCommand(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseGoalCommand(%v) expected error", tc.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseGoalCommand(%v): %v", tc.args, err)
			continue
		}
		if cmd.Verb != tc.verb || cmd.Objective != tc.obj {
			t.Errorf("ParseGoalCommand(%v) = %+v, want verb=%s obj=%s", tc.args, cmd, tc.verb, tc.obj)
		}
	}
	if cmd, _ := ParseGoalCommand([]string{"set", "--cycles", "3", "x"}); cmd.MaxCycles != 3 {
		t.Fatalf("--cycles not parsed: %+v", cmd)
	}
}

func TestGoalSubscriptionNotifies(t *testing.T) {
	a := newGoalAgent(t)
	seen := []*Goal{}
	unsub := a.SubscribeGoal(func(g *Goal) { seen = append(seen, g) })
	a.SetGoal("watched", "", 2, 1000)
	a.PauseGoal()
	unsub()
	a.ResumeGoal()
	if len(seen) != 2 {
		t.Fatalf("notifications after unsubscribe: %d (want 2)", len(seen))
	}
	if seen[0] == nil || seen[0].Objective != "watched" || seen[0].Status != GoalActive {
		t.Fatalf("first notification = %+v", seen[0])
	}
	if seen[1].Status != GoalPaused {
		t.Fatalf("second notification = %+v", seen[1])
	}
}

func TestGoalBlockedByModelEndsLoop(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess 1"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "1", Name: "UpdateGoal",
			Args: map[string]any{"action": "blocked", "reason": "missing API key"},
		}}},
		messages.AI("cannot proceed"),
		messages.AI(`{"verdict":"continue","gaps":"blocked on creds"}`))
	a.SetGoal("needs creds", "", 5, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resg := pumpGoalChunks(t, stream)
	if resg.err != nil || len(resg.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", resg.err)
	}
	if g := a.Goal(); g.Status != GoalBlocked || g.BlockedReason != "missing API key" {
		t.Fatalf("goal = %+v", g)
	}
}
