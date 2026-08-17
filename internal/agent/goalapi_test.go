package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/obslog"
)

func TestSetGoalValidation(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	if _, err := a.SetGoal("   ", "", 0, 0); err == nil {
		t.Fatal("blank objective must error")
	}
	// A running loop refuses replacement.
	a.goal.mu.Lock()
	a.goal.running = true
	a.goal.mu.Unlock()
	if _, err := a.SetGoal("another", "", 1, 1); err == nil {
		t.Fatal("SetGoal during a running loop must error")
	}
	a.goal.mu.Lock()
	a.goal.running = false
	a.goal.mu.Unlock()

	// Non-positive budgets fall back to the defaults.
	g, err := a.SetGoal("objective", "sess", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxCycles != DefaultGoalMaxCycles || g.MaxTokens != DefaultGoalMaxTokens {
		t.Fatalf("defaults = %d/%d", g.MaxCycles, g.MaxTokens)
	}
	if g.SessionID != "sess" || g.Status != GoalActive {
		t.Fatalf("goal = %+v", g)
	}
}

func TestRestoreGoalValidation(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	if err := a.RestoreGoal(nil); err == nil {
		t.Fatal("nil restore must error")
	}
	if err := a.RestoreGoal(&Goal{Objective: "  "}); err == nil {
		t.Fatal("blank objective restore must error")
	}
	// Running loop refuses.
	a.goal.mu.Lock()
	a.goal.running = true
	a.goal.mu.Unlock()
	if err := a.RestoreGoal(&Goal{Objective: "x"}); err == nil {
		t.Fatal("restore during running loop must error")
	}
	a.goal.mu.Lock()
	a.goal.running = false
	a.goal.mu.Unlock()
	// An existing goal refuses.
	a.SetGoal("current", "", 1, 1)
	if err := a.RestoreGoal(&Goal{Objective: "x"}); err == nil {
		t.Fatal("restore over an existing goal must error")
	}
	a.ClearGoal()

	// A fresh process cannot resume mid-loop: active goals restore paused.
	g := &Goal{Objective: "restored", Status: GoalActive, TokensUsed: 5}
	if err := a.RestoreGoal(g); err != nil {
		t.Fatal(err)
	}
	got := a.Goal()
	if got.Status != GoalPaused {
		t.Fatalf("restored status = %q, want paused", got.Status)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("restore must stamp CreatedAt/UpdatedAt")
	}
	if !a.ResumeGoal() {
		t.Fatal("resume of restored goal failed")
	}
}

func TestGoalManagerEdgeTransitions(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))

	// No-goal guards.
	if a.PauseGoal() {
		t.Fatal("PauseGoal without a goal must fail")
	}
	if a.GoalSetBudget(5, 100) {
		t.Fatal("GoalSetBudget without a goal must fail")
	}
	if a.goalClaimCompletion("e") {
		t.Fatal("claim without a goal must fail")
	}
	if a.goalMarkBlocked("r") {
		t.Fatal("blocked without a goal must fail")
	}
	if a.goalSetStatus(GoalComplete, "", "e") != nil {
		t.Fatal("setStatus without a goal must return nil")
	}
	a.goalAddNote("orphan") // no goal: no-op, no panic
	if a.goalStall() {
		t.Fatal("stall without a goal must report false")
	}

	a.SetGoal("managed", "", 3, 1000)

	// Budget updates apply and notify.
	if !a.GoalSetBudget(7, 5000) {
		t.Fatal("GoalSetBudget failed")
	}
	if g := a.Goal(); g.MaxCycles != 7 || g.MaxTokens != 5000 {
		t.Fatalf("budgets = %d/%d", g.MaxCycles, g.MaxTokens)
	}
	// Zero values keep the current budgets.
	a.GoalSetBudget(0, 0)
	if g := a.Goal(); g.MaxCycles != 7 || g.MaxTokens != 5000 {
		t.Fatalf("zero budget changed values: %d/%d", g.MaxCycles, g.MaxTokens)
	}

	// Notes: blank skipped, capped at 20.
	a.goalAddNote("   ")
	if g := a.Goal(); len(g.Notes) != 0 {
		t.Fatalf("blank note recorded: %+v", g.Notes)
	}
	for i := 0; i < 25; i++ {
		a.goalAddNote("note")
	}
	if g := a.Goal(); len(g.Notes) != 20 {
		t.Fatalf("notes = %d, want 20", len(g.Notes))
	}

	// Claim + blocked on an inactive goal fails.
	a.PauseGoal()
	if a.goalMarkBlocked("why") {
		t.Fatal("blocked on a paused goal must fail")
	}
	if !a.ResumeGoal() {
		t.Fatal("resume failed")
	}
	if !a.goalClaimCompletion("evidence") {
		t.Fatal("claim failed")
	}
	if g := a.Goal(); !g.Claimed || g.ClaimEvidence != "evidence" {
		t.Fatalf("claim = %+v", g)
	}
	if !a.goalMarkBlocked("stuck") {
		t.Fatal("blocked failed")
	}

	// goalSetStatus branches for each terminal state.
	a.SetGoal("statuses", "", 3, 1000)
	a.goalSetStatus(GoalComplete, "", "ev")
	if g := a.Goal(); g.Status != GoalComplete || g.Evidence != "ev" {
		t.Fatalf("complete = %+v", g)
	}
	a.ClearGoal()
	a.SetGoal("statuses", "", 3, 1000)
	a.goalSetStatus(GoalBlocked, "why", "")
	if g := a.Goal(); g.Status != GoalBlocked || g.BlockedReason != "why" {
		t.Fatalf("blocked = %+v", g)
	}
	a.ClearGoal()
	a.SetGoal("statuses", "", 3, 1000)
	a.goalSetStatus(GoalBudgetLimited, "spent", "")
	if g := a.Goal(); g.Status != GoalBudgetLimited || g.BlockedReason != "spent" {
		t.Fatalf("budget = %+v", g)
	}
	// An exhausted budget can be resumed.
	if !a.ResumeGoal() {
		t.Fatal("resume of budget-limited goal failed")
	}
	if !a.ClearGoal() {
		t.Fatal("ClearGoal of a set goal must report true")
	}
	if a.ClearGoal() {
		t.Fatal("ClearGoal twice must report false")
	}
}

func TestGoalRunBookkeeping(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	a.SetGoal("bk", "", 5, 1_000_000)

	if a.GoalRunID() != 0 {
		t.Fatal("GoalRunID must be 0 when no run is in flight")
	}
	a.AbandonGoalRun(0) // no-op
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the stream referenced: the run stays in flight until abandoned.
	_ = stream
	run := a.GoalRunID()
	if run == 0 {
		t.Fatal("GoalRunID must be non-zero during a run")
	}
	// A stale run id does not clear the bookkeeping.
	a.AbandonGoalRun(run + 1000)
	if a.GoalRunID() != run {
		t.Fatal("stale abandon cleared the run")
	}
	// The current run id does.
	a.AbandonGoalRun(run)
	if a.GoalRunID() != 0 {
		t.Fatal("abandon did not clear the run")
	}
}

func TestGoalStreamGuardErrors(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	if _, err := a.GoalStream(context.Background(), nil); err == nil {
		t.Fatal("GoalStream without a goal must error")
	}
	a.SetGoal("g", "", 2, 1000)
	a.PauseGoal()
	if _, err := a.GoalStream(context.Background(), nil); err == nil {
		t.Fatal("GoalStream on a paused goal must error")
	}
	a.ResumeGoal()
	// A run already in flight refuses a second stream.
	a.goal.mu.Lock()
	a.goal.running = true
	a.goal.graph = a.buildGoalGraph(1)
	a.goal.mu.Unlock()
	if _, err := a.GoalStream(context.Background(), nil); err == nil {
		t.Fatal("GoalStream while running must error")
	}
	// GoalResume without a live inner stream errors.
	a.goal.mu.Lock()
	a.goal.running = false
	a.goal.graph = nil
	a.goal.mu.Unlock()
	if _, err := a.GoalResume(context.Background(), true); err == nil {
		t.Fatal("GoalResume without a run must error")
	}
}

func TestGoalBudgetLimitedByTokens(t *testing.T) {
	assess := messages.AI("plan")
	assess.UsageMetadata = messages.UsageMetadata{InputTokens: 900, OutputTokens: 100, TotalTokens: 1000}
	a := newGoalAgent(t,
		assess,
		messages.AI("work"),
		messages.AI(`{"verdict":"continue","gaps":"more"}`),
	)
	a.SetGoal("tiny budget", "", 10, 50) // 50-token cap
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := pumpGoalChunks(t, stream)
	if res.err != nil || len(res.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", res.err)
	}
	g := a.Goal()
	if g.Status != GoalBudgetLimited {
		t.Fatalf("status = %q, want budget_limited (tokens)", g.Status)
	}
	if !strings.Contains(g.BlockedReason, "token budget reached") {
		t.Fatalf("reason = %q", g.BlockedReason)
	}
}

func TestGoalModelErrorEndsRun(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	a.SetGoal("boom", "", 5, 1_000_000)
	// Swap in a model that always fails; the assess call errors and the
	// stream's error step ends the run bookkeeping.
	a.mu.Lock()
	a.boundModel = erroringSubagentModel{nil}
	a.mu.Unlock()
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for _, cerr := range stream {
		if cerr != nil {
			streamErr = cerr
			break
		}
	}
	if streamErr == nil {
		t.Fatal("expected the model error to surface")
	}
	if a.GoalRunning() {
		t.Fatal("run bookkeeping not cleared on error")
	}
}

func TestParseGoalVerdictLenient(t *testing.T) {
	cases := []struct {
		in      string
		verdict string
	}{
		{`{"verdict":"complete","evidence":"e"}`, "complete"},
		{`prose {"verdict":"blocked","gaps":"g"} trailing`, "blocked"},
		{`{"verdict":"weird"}`, "continue"}, // unknown verdict coerced
		{`not json at all`, "continue"},     // unparsable → continue
		{`{"verdict": 42}`, "continue"},     // type error → continue
		{`{broken`, "continue"},             // no closing brace
	}
	for _, tc := range cases {
		v := parseGoalVerdict(tc.in)
		if v.Verdict != tc.verdict {
			t.Errorf("parseGoalVerdict(%q) = %q, want %q", tc.in, v.Verdict, tc.verdict)
		}
	}
	// No braces at all keeps a truncated copy as evidence.
	if v := parseGoalVerdict("plain text"); v.Verdict != "continue" || v.Evidence != "plain text" {
		t.Fatalf("plain = %+v", v)
	}
}

func TestGoalFormattingHelpers(t *testing.T) {
	if got := FormatGoalTokens(1_500_000); got != "1M" {
		t.Fatalf("FormatGoalTokens = %q", got)
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{1_500_000, "1M"}, {2_000_000, "2M"}, {120_000, "120k"}, {999, "999"},
	} {
		if got := formatTokens(tc.n); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	if got := goalTruncate("  hello  ", 20); got != "hello" {
		t.Fatalf("goalTruncate trim = %q", got)
	}
	if got := goalTruncate("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("goalTruncate cut = %q", got)
	}
	if got := firstLine("a\nb"); got != "a" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("solo"); got != "solo" {
		t.Fatalf("firstLine solo = %q", got)
	}
	if got := orString("", "fallback"); got != "fallback" {
		t.Fatalf("orString = %q", got)
	}
	if got := orString("first", "second"); got != "first" {
		t.Fatalf("orString first = %q", got)
	}

	// goalReport renders every terminal status.
	for _, st := range []GoalStatus{GoalComplete, GoalBlocked, GoalPaused, GoalBudgetLimited, "other"} {
		if got := goalReport(st, goalVerdict{}, 3); got == "" {
			t.Fatalf("goalReport(%s) empty", st)
		}
	}

	// goalMessageTail bounds and formats roles.
	empty := goalMessageTail(nil, 5, 10)
	if empty != "(empty)" {
		t.Fatalf("empty tail = %q", empty)
	}
	msgs := []messages.Message{
		messages.Human("user text"),
		{Role: messages.RoleAI, Content: "ai text", ToolCalls: []messages.ToolCall{{Name: "Read", Args: map[string]any{"file_path": "x"}}}},
		messages.Tool("t1", "tool output"),
	}
	tail := goalMessageTail(msgs, 3, 40)
	if !strings.Contains(tail, "user text") || !strings.Contains(tail, "assistant called Read") ||
		!strings.Contains(tail, "tool result") {
		t.Fatalf("tail = %q", tail)
	}
	// Only the last max messages are rendered.
	var many []messages.Message
	for i := 0; i < 10; i++ {
		many = append(many, messages.Human("m"))
	}
	if got := goalMessageTail(many, 3, 10); strings.Count(got, "user: m") != 3 {
		t.Fatalf("tail not bounded: %q", got)
	}

	// goalCycleMessages slices from the last cycle marker (or the tail).
	cycle := messages.Human("<goal-cycle 1>\nwork\n</goal-cycle>")
	sliced := goalCycleMessages([]messages.Message{messages.Human("old"), cycle, messages.AI("after")})
	if len(sliced) != 2 || sliced[0].Content != cycle.Content {
		t.Fatalf("cycle slice = %+v", sliced)
	}
	var bulk []messages.Message
	for i := 0; i < 40; i++ {
		bulk = append(bulk, messages.Human("m"))
	}
	if got := goalCycleMessages(bulk); len(got) != 30 {
		t.Fatalf("fallback slice = %d, want 30", len(got))
	}
	if got := goalCycleMessages(bulk[:5]); len(got) != 5 {
		t.Fatalf("short slice = %d", len(got))
	}
}

func TestFormatGoalRendering(t *testing.T) {
	if got := FormatGoal(nil); !strings.Contains(got, "no goal set") {
		t.Fatalf("nil goal = %q", got)
	}
	base := &Goal{
		Objective: "ship it", Status: GoalActive, Cycles: 2, MaxCycles: 5,
		TokensUsed: 1500, MaxTokens: 2000, Claimed: true,
	}
	if got := FormatGoal(base); !strings.Contains(got, "claim awaiting verification") {
		t.Fatalf("claimed render = %q", got)
	}
	blocked := *base
	blocked.Status = GoalBlocked
	blocked.BlockedReason = "no creds"
	if got := FormatGoal(&blocked); !strings.Contains(got, "blocked: no creds") {
		t.Fatalf("blocked render = %q", got)
	}
	complete := *base
	complete.Status = GoalComplete
	complete.Evidence = "tests green"
	if got := FormatGoal(&complete); !strings.Contains(got, "evidence: tests green") {
		t.Fatalf("complete render = %q", got)
	}
	limited := *base
	limited.Status = GoalBudgetLimited
	limited.BlockedReason = "spent"
	if got := FormatGoal(&limited); !strings.Contains(got, "/goal budget") {
		t.Fatalf("budget render = %q", got)
	}
	paused := *base
	paused.Status = GoalPaused
	if got := FormatGoal(&paused); !strings.Contains(got, "/goal resume") {
		t.Fatalf("paused render = %q", got)
	}
	// Notes and the last five progress entries render.
	withHistory := &Goal{
		Objective: "o", Status: GoalActive, MaxCycles: 9,
		Notes: []string{"note"},
		Progress: []GoalProgressEntry{
			{Cycle: 1, Verdict: "continue"},
			{Cycle: 2, Verdict: "continue", Summary: "did stuff"},
			{Cycle: 3, Verdict: "continue", Note: "note only"},
			{Cycle: 4, Verdict: "continue", Summary: "s4"},
			{Cycle: 5, Verdict: "continue", Summary: "s5"},
			{Cycle: 6, Verdict: "continue", Summary: "s6"},
			{Cycle: 7, Verdict: "continue", Summary: "s7"},
		},
	}
	got := FormatGoal(withHistory)
	for _, want := range []string{"note", "s7", "s6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	// The window holds only the last five cycles (3-7); cycles 1-2 are trimmed.
	if strings.Contains(got, "did stuff") || strings.Contains(got, "  1 [") || strings.Contains(got, "  2 [") {
		t.Fatalf("old cycle leaked into window:\n%s", got)
	}
}

func TestParseGoalCommandBudgetAndFlags(t *testing.T) {
	if cmd, err := ParseGoalCommand([]string{"help"}); err != nil || cmd.Verb != "help" {
		t.Fatalf("help = %+v, %v", cmd, err)
	}
	if cmd, err := ParseGoalCommand([]string{"continue"}); err != nil || cmd.Verb != "resume" {
		t.Fatalf("continue = %+v, %v", cmd, err)
	}
	if cmd, err := ParseGoalCommand([]string{"stop"}); err != nil || cmd.Verb != "clear" {
		t.Fatalf("stop = %+v, %v", cmd, err)
	}
	if cmd, err := ParseGoalCommand([]string{"start", "going"}); err != nil || cmd.Verb != "set" || cmd.Objective != "going" {
		t.Fatalf("start = %+v, %v", cmd, err)
	}
	for _, bad := range [][]string{
		{"budget"},
		{"budget", "cycles"},
		{"budget", "cycles", "0"},
		{"budget", "cycles", "x"},
		{"budget", "wat", "5"},
	} {
		if _, err := ParseGoalCommand(bad); err == nil {
			t.Errorf("ParseGoalCommand(%v) should error", bad)
		}
	}
	cmd, err := ParseGoalCommand([]string{"budget", "tokens", "12345"})
	if err != nil || cmd.BudgetKey != "tokens" || cmd.BudgetVal != 12345 {
		t.Fatalf("budget tokens = %+v, %v", cmd, err)
	}
	cmd, err = ParseGoalCommand([]string{"set", "--tokens", "999", "obj"})
	if err != nil || cmd.MaxTokens != 999 || cmd.Objective != "obj" {
		t.Fatalf("set --tokens = %+v, %v", cmd, err)
	}
	for _, bad := range [][]string{
		{"set", "--tokens"},
		{"set", "--tokens", "bad", "o"},
		{"set", "--tokens", "0", "o"},
		{"set", "--cycles", "bad", "o"},
		{"set", "--cycles", "0", "o"},
	} {
		if _, err := ParseGoalCommand(bad); err == nil {
			t.Errorf("ParseGoalCommand(%v) should error", bad)
		}
	}
}

func TestGoalUpdateToolValidation(t *testing.T) {
	a := newGoalAgent(t, messages.AI("ok"))
	// Every action without a goal reports an error result (not a Go error).
	for _, args := range []map[string]any{
		{"action": "progress", "note": "n"},
		{"action": "claim_complete", "evidence": "e"},
		{"action": "blocked", "reason": "r"},
	} {
		res := a.execute(context.Background(), "UpdateGoal", args)
		if !strings.Contains(res.Content, "Error") {
			t.Fatalf("UpdateGoal without goal = %q", res.Content)
		}
	}
	a.SetGoal("toolful", "", 5, 1000)
	// Field validation with an active goal.
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "progress"}); !strings.Contains(res.Content, "requires a note") {
		t.Fatalf("progress without note = %q", res.Content)
	}
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "claim_complete"}); !strings.Contains(res.Content, "requires evidence") {
		t.Fatalf("claim without evidence = %q", res.Content)
	}
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "blocked"}); !strings.Contains(res.Content, "requires a reason") {
		t.Fatalf("blocked without reason = %q", res.Content)
	}
	// Blocked falls back to the note for its reason, then deactivates the goal.
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "blocked", "note": "via note"}); !strings.Contains(res.Content, "via note") {
		t.Fatalf("blocked via note = %q", res.Content)
	}
	if g := a.Goal(); g.Status != GoalBlocked {
		t.Fatalf("status = %q", g.Status)
	}
	// Further blocked actions on the inactive goal fail.
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "blocked", "reason": "again"}); !strings.Contains(res.Content, "not active") {
		t.Fatalf("blocked on inactive = %q", res.Content)
	}
	// Case/whitespace-insensitive action matching.
	a.SetGoal("again", "", 5, 1000)
	if res := a.execute(context.Background(), "UpdateGoal", map[string]any{"action": "  PROGRESS ", "note": "n"}); !strings.Contains(res.Content, "Progress noted") {
		t.Fatalf("progress = %q", res.Content)
	}
	// GetGoal renders the goal as JSON.
	res := a.execute(context.Background(), "GetGoal", nil)
	if !strings.Contains(res.Content, `"objective": "again"`) && !strings.Contains(res.Content, "again") {
		t.Fatalf("GetGoal = %q", res.Content)
	}
}

func TestGoalGraphWithObserver(t *testing.T) {
	// buildGoalGraph wires the observer when obs is configured; the loop
	// still runs to completion.
	a := newGoalAgent(t,
		messages.AI("assess"),
		messages.AI("work"),
		messages.AI(`{"verdict":"complete","evidence":"done"}`))
	a.obs = obslog.New(t.TempDir(), false)
	a.SetGoal("observed", "", 5, 1_000_000)
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := pumpGoalChunks(t, stream)
	if res.err != nil || len(res.interrupts) > 0 {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if g := a.Goal(); g.Status != GoalComplete {
		t.Fatalf("status = %q", g.Status)
	}
}

func TestGoalStampsAndZeroTurns(t *testing.T) {
	a := newGoalAgent(t,
		messages.AI("assess"),
		messages.AI("work"),
		messages.AI(`{"verdict":"complete","evidence":"ok"}`))
	a.SetGoal("timing", "", 5, 1_000_000)
	before := time.Now()
	stream, err := a.GoalStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pumpGoalChunks(t, stream)
	g := a.Goal()
	if g.UpdatedAt.Before(before) {
		t.Fatal("UpdatedAt not refreshed")
	}
}
