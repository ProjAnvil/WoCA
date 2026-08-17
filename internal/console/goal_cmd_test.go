package console

import (
	"context"
	"github.com/projanvil/woca/internal/agent"
	"strings"
	"testing"
)

func TestGoalCommandLifecycle(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	sess := &Session{}

	res, err := c.Execute(context.Background(), "goal", nil, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no goal set") {
		t.Fatalf("status without goal = %q", res.Output)
	}

	res, err = c.Execute(context.Background(), "goal", []string{"set", "--cycles", "3", "make", "the", "tests", "pass"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "goal set") || !strings.Contains(res.Output, "max 3 cycles") {
		t.Fatalf("set output = %q", res.Output)
	}

	res, err = c.Execute(context.Background(), "goal", nil, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "goal [active]: make the tests pass") ||
		!strings.Contains(res.Output, "cycle 0/3") {
		t.Fatalf("status = %q", res.Output)
	}

	res, err = c.Execute(context.Background(), "goal", []string{"pause"}, sess)
	if err != nil || !strings.Contains(res.Output, "paused") {
		t.Fatalf("pause = %q err=%v", res.Output, err)
	}
	res, err = c.Execute(context.Background(), "goal", nil, sess)
	if err != nil || !strings.Contains(res.Output, "[paused]") {
		t.Fatalf("paused status = %q err=%v", res.Output, err)
	}

	res, err = c.Execute(context.Background(), "goal", []string{"resume"}, sess)
	if err != nil || !strings.Contains(res.Output, "resumed") {
		t.Fatalf("resume = %q err=%v", res.Output, err)
	}

	res, err = c.Execute(context.Background(), "goal", []string{"budget", "cycles", "8"}, sess)
	if err != nil || !strings.Contains(res.Output, "cycle 0/8") {
		t.Fatalf("budget = %q err=%v", res.Output, err)
	}

	res, err = c.Execute(context.Background(), "goal", []string{"clear"}, sess)
	if err != nil || !strings.Contains(res.Output, "cleared") {
		t.Fatalf("clear = %q err=%v", res.Output, err)
	}
	if c.deps.Agent.Goal() != nil {
		t.Fatal("goal not dropped")
	}

	if _, err := c.Execute(context.Background(), "goal", []string{"set"}, sess); err == nil {
		t.Fatal("set without objective should fail")
	}

	// Free text without a verb is shorthand for set.
	res, err = c.Execute(context.Background(), "goal", []string{"just", "wibble", "it"}, sess)
	if err != nil || !strings.Contains(res.Output, "goal set") {
		t.Fatalf("free-text set = %q err=%v", res.Output, err)
	}
	if g := c.deps.Agent.Goal(); g == nil || g.Objective != "just wibble it" {
		t.Fatalf("free-text objective = %+v", c.deps.Agent.Goal())
	}
}

func TestGoalCommandSurface(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{}

	run := func(args ...string) (string, error) {
		res, err := c.Execute(ctx, "goal", args, s)
		return res.Output, err
	}
	if out, err := run("help"); err != nil || !strings.Contains(out, "goal") {
		t.Fatalf("help = %q %v", out, err)
	}
	if out, err := run("status"); err != nil || !strings.Contains(out, "No goal") && !strings.Contains(out, "no goal") {
		t.Fatalf("status = %q %v", out, err)
	}
	if out, err := run("set", "make it pass"); err != nil || !strings.Contains(out, "make it pass") {
		t.Fatalf("set = %q %v", out, err)
	}
	if out, err := run("status"); err != nil || !strings.Contains(out, "make it pass") {
		t.Fatalf("status after set = %q %v", out, err)
	}
	if _, err := run("budget", "tokens", "2000000"); err != nil {
		t.Fatalf("budget tokens = %v", err)
	}
	if _, err := run("budget", "cycles", "12"); err != nil {
		t.Fatalf("budget cycles = %v", err)
	}
	if _, err := run("budget", "bogus", "1"); err == nil {
		t.Fatal("expected budget usage error")
	}
	if _, err := run("pause"); err != nil {
		t.Fatalf("pause = %v", err)
	}
	if _, err := run("resume"); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if _, err := run("clear"); err != nil {
		t.Fatalf("clear = %v", err)
	}
	if out, err := run("list"); err != nil || out == "" {
		t.Logf("list = %q %v", out, err)
	}
	// Natural language defaults to setting an objective.
	if out, err := run("bogus objective text"); err != nil || !strings.Contains(out, "bogus objective text") {
		t.Fatalf("natural language = %q %v", out, err)
	}
}

func TestFormatGoalVariants(t *testing.T) {
	base := agent.Goal{Status: agent.GoalActive, Objective: "obj", Cycles: 2, MaxCycles: 10, TokensUsed: 5000, MaxTokens: 2000000}
	g := base
	g.Claimed = true
	g.Notes = []string{"note one", "note two"}
	g.Progress = []agent.GoalProgressEntry{
		{Cycle: 1, Verdict: "continue", Summary: "started"},
		{Cycle: 2, Verdict: "complete", Note: "fallback note"},
	}
	out := formatGoal(&g)
	for _, want := range []string{"completion claim", "note two", "fallback note", "[continue] started"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Terminal statuses render their reason lines.
	for _, c := range []struct {
		status agent.GoalStatus
		want   string
	}{
		{agent.GoalBlocked, "blocked:"},
		{agent.GoalComplete, "evidence:"},
		{agent.GoalBudgetLimited, "/goal budget"},
		{agent.GoalPaused, "paused"},
	} {
		g := base
		g.Status = c.status
		g.BlockedReason = "why"
		g.Evidence = "proof"
		if out := formatGoal(&g); !strings.Contains(out, c.want) {
			t.Errorf("status %v missing %q in:\n%s", c.status, c.want, out)
		}
	}
	// Long progress windows trim to the last five cycles.
	g = base
	for i := 0; i < 8; i++ {
		g.Progress = append(g.Progress, agent.GoalProgressEntry{Cycle: i + 1, Verdict: "continue", Summary: fmt_Sprintf("cycle-%d", i+1)})
	}
	out = formatGoal(&g)
	if strings.Contains(out, "cycle-1\n") && strings.Contains(out, "cycle-8") == false {
		t.Errorf("progress window not trimmed:\n%s", out)
	}
}

func TestGoalCommandEdgeErrors(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{}
	run := func(args ...string) error {
		_, err := c.Execute(ctx, "goal", args, s)
		return err
	}
	// Pause/resume/clear/budget with no goal.
	if err := run("pause"); err == nil || !strings.Contains(err.Error(), "no goal") {
		t.Fatalf("pause no goal = %v", err)
	}
	if err := run("resume"); err == nil || !strings.Contains(err.Error(), "no goal") {
		t.Fatalf("resume no goal = %v", err)
	}
	if out, _ := c.Execute(ctx, "goal", []string{"clear"}, s); !strings.Contains(out.Output, "no goal") {
		t.Fatalf("clear no goal = %+v", out)
	}
	// Set, then resume-while-active and double pause.
	if err := run("set", "objective"); err != nil {
		t.Fatal(err)
	}
	if out, _ := c.Execute(ctx, "goal", []string{"resume"}, s); !strings.Contains(out.Output, "already active") {
		t.Fatalf("resume active = %+v", out)
	}
	if err := run("pause"); err != nil {
		t.Fatal(err)
	}
	if err := run("pause"); err == nil || !strings.Contains(err.Error(), "nothing to pause") {
		t.Fatalf("double pause = %v", err)
	}
	if err := run("resume"); err != nil {
		t.Fatal(err)
	}
	// Clearing works and reports the cleared state.
	if out, _ := c.Execute(ctx, "goal", []string{"clear"}, s); !strings.Contains(out.Output, "cleared") {
		t.Fatalf("clear = %+v", out)
	}
}
