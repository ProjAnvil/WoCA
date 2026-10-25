package console

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/quota"
)

func TestThinkingCommand(t *testing.T) {
	set := 0
	c, _, _ := newTestConsole(t, nil)
	c.deps.CfgLive = func() config.Config {
		return config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5", ThinkingBudget: set}
	}
	c.deps.SetThinkingBudget = func(b int) error { set = b; return nil }

	// View.
	res, err := c.Execute(t.Context(), "thinking", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "off") {
		t.Fatalf("view output = %q", res.Output)
	}

	// Named levels and raw budgets round-trip through the callback.
	for arg, want := range map[string]int{
		"low": config.ThinkingBudgetLow, "medium": config.ThinkingBudgetMedium,
		"high": config.MaxThinkingBudget, "off": 0, "4096": 4096,
	} {
		res, err := c.Execute(t.Context(), "thinking", []string{arg}, &Session{})
		if err != nil {
			t.Fatalf("thinking %s: %v", arg, err)
		}
		if set != want {
			t.Fatalf("thinking %s: budget = %d, want %d", arg, set, want)
		}
		if res.Output == "" {
			t.Fatalf("thinking %s: empty output", arg)
		}
	}

	// Invalid input is rejected.
	if _, err := c.Execute(t.Context(), "thinking", []string{"ultrathink"}, &Session{}); err == nil {
		t.Fatal("invalid level accepted")
	}
	if _, err := c.Execute(t.Context(), "thinking", []string{"100"}, &Session{}); err == nil {
		t.Fatal("too-small budget accepted")
	}

	// Models without a thinking mode refuse a non-zero budget…
	c.deps.CfgLive = func() config.Config {
		return config.Config{Provider: config.ProviderOpenAI, Model: "gpt-4o"}
	}
	if _, err := c.Execute(t.Context(), "thinking", []string{"low"}, &Session{}); err == nil {
		t.Fatal("gpt-4o thinking accepted")
	}
	// …while the OpenAI reasoning family accepts the switch.
	c.deps.CfgLive = func() config.Config {
		return config.Config{Provider: config.ProviderOpenAI, Model: "o3"}
	}
	if _, err := c.Execute(t.Context(), "thinking", []string{"low"}, &Session{}); err != nil {
		t.Fatalf("o3 thinking low: %v", err)
	}

	// A missing callback degrades switching but keeps viewing.
	c.deps.CfgLive = func() config.Config {
		return config.Config{Provider: config.ProviderOpenAI, Model: "o3"}
	}
	c.deps.SetThinkingBudget = nil
	if _, err := c.Execute(t.Context(), "thinking", []string{"low"}, &Session{}); err == nil {
		t.Fatal("switching without callback accepted")
	}
	res, err = c.Execute(t.Context(), "thinking", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "thinking=") {
		t.Fatalf("view output = %q", res.Output)
	}
}

func TestQuotaCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)

	// No callback: helpful message, not an error.
	res, err := c.Execute(t.Context(), "quota", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "not available") {
		t.Fatalf("quota without dep: res=%+v err=%v", res, err)
	}

	c.deps.Quota = func(ctx context.Context) (quota.Info, error) {
		return quota.Info{Available: true, Level: "pro", Windows: []quota.Window{
			{Kind: "5h", UsedPercent: 42},
		}}, nil
	}
	res, err = c.Execute(t.Context(), "quota", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pro", "5h", "58.0%"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("quota output missing %q:\n%s", want, res.Output)
		}
	}
}
