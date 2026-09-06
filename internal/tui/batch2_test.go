package tui

import (
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/skills"
)

// TestSlashMenuMergesSkills checks the "/"-trigger grammar: console commands
// and loaded skills share one grouped menu, and accepting a skill entry
// completes the line (which then dispatches as a Skill-tool turn).
func TestSlashMenuMergesSkills(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m.agent.SetSkills([]skills.Skill{{Name: "pdf", Description: "pdf tools"}, {Name: "code-review", Description: "review a change"}})
	m.agent.SetAgentDefinitions(nil)
	m.refreshCompletions()

	m.input = "/pd"
	menu := m.allSuggestions(m.input)
	if len(menu) != 1 || menu[0].name != "/pdf" || menu[0].group != groupSkills {
		t.Fatalf("skill menu = %+v", menu)
	}
	// The command group still matches.
	m.input = "/to"
	menu = m.allSuggestions(m.input)
	found := false
	for _, c := range menu {
		if c.name == "/todos" && c.group == groupCommands {
			found = true
		}
	}
	if !found {
		t.Fatalf("commands missing from the merged menu: %+v", menu)
	}
	// The rendered view shows both group headers when an input matches
	// commands and skills at once.
	m.input = "/co"
	v := m.View()
	if !strings.Contains(v, "commands") || !strings.Contains(v, "skills") || !strings.Contains(v, "review a change") {
		t.Fatalf("menu rendering missing groups:\n%s", v)
	}
}

// TestAtTriggerCompletesAgents checks the "@"-trigger grammar: at line start
// or after a space (not mid-word), completing inserts "@name " and a second
// enter submits the whole line as a prompt.
func TestAtTriggerCompletesAgents(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m.cfg.WorkDir = t.TempDir()
	m.agent.SetAgentDefinitions([]agents.Definition{{Name: "reviewer", Description: "reviews diffs"}})
	m.refreshCompletions()

	m.input = "ask @rev"
	menu := m.allSuggestions(m.input)
	if len(menu) != 1 || menu[0].name != "@reviewer" || menu[0].group != groupAgents {
		t.Fatalf("agent menu = %+v", menu)
	}
	// A mid-word @ is not a trigger.
	m.input = "email@ex"
	if got := m.allSuggestions(m.input); got != nil {
		t.Fatalf("mid-word @ must not trigger: %+v", got)
	}
	// Tab accepts the highlighted entry with a trailing space.
	m.input = "ask @rev"
	m = press(m, ctrlKey("tab"))
	if m.input != "ask @reviewer " {
		t.Fatalf("accepted completion = %q", m.input)
	}
	// Enter submits the prompt (no menu left).
	m = press(m, key("enter"))
	if !m.running || !strings.Contains(m.history[len(m.history)-1].Content, "ask @reviewer") {
		t.Fatalf("completed line not submitted: running=%v", m.running)
	}
}

// TestAtTriggerDetection pins the grammar edge cases.
func TestAtTriggerDetection(t *testing.T) {
	cases := map[string]int{
		"@":         0,
		"@par":      0,
		"ask @":     4,
		"ask @rev":  4,
		"a@b":       -1,
		"@rev x":    -1, // token not trailing
		"":          -1,
		"plain":     -1,
		"@rev-word": -1, // hyphen is part of the token charset
	}
	for input, want := range cases {
		// "@rev-word" contains '-' which IS allowed; recompute expectation.
		got := atTrigger(input)
		if input == "@rev-word" {
			want = 0
		}
		if got != want {
			t.Errorf("atTrigger(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestContextFooterAfterRun drives a run and checks the footer segment and
// the danger hint threshold rendering helper.
func TestContextFooterAfterRun(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m = driveRun(t, m, "hello")
	if m.ctxInfo == nil {
		t.Fatal("runDone must refresh the context snapshot")
	}
	if v := m.View(); !strings.Contains(v, "ctx") || !strings.Contains(v, "%") {
		t.Fatalf("view missing the context segment:\n%s", v)
	}
}

// TestCtxFooterThresholds checks the bar math and thresholds.
func TestCtxFooterThresholds(t *testing.T) {
	if got := ctxFooter(nil); got != "" {
		t.Fatalf("nil info footer = %q", got)
	}
	info := &agent.ContextInfo{EstimatedTotal: 50_000, ContextWindow: 100_000}
	seg := ctxFooter(info)
	if !strings.Contains(seg, "50%") || !strings.Contains(seg, "50k/100k") || !strings.Contains(seg, "▓▓▓▓▓░░░░░") {
		t.Fatalf("footer = %q", seg)
	}
}
