package console

import (
	"context"
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
	if _, err := c.Execute(context.Background(), "goal", []string{"wibble"}, sess); err == nil {
		t.Fatal("unknown subcommand should fail")
	}
}
