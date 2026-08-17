package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

func TestInitStartsSpinner(t *testing.T) {
	m := New(nil, nil, config.Config{})
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init must return the spinner tick command")
	}
	mm, next := m.Update(cmd())
	if mm == nil || next == nil {
		t.Fatalf("spinner tick did not schedule the next tick: %v", next)
	}
}

func TestRunDoneMessage(t *testing.T) {
	a := newFakeAgent(t, messages.AI("ok"))

	// Error tail: the run ends, the fragment flushes, the error is echoed.
	m := New(a, nil, config.Config{})
	m.running = true
	m.ch = make(chan tea.Msg, 1)
	m.write("partial")
	mm, _ := m.Update(runDone{err: errors.New("boom")})
	m = mm.(*Model)
	if m.running || m.ch != nil {
		t.Fatalf("after a failed run: running=%v ch=%v", m.running, m.ch != nil)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "Error: boom") {
		t.Fatalf("transcript missing the run error:\n%s", strings.Join(m.lines, "\n"))
	}

	// Success tail: the system prompt is stripped, todos refresh, no goal.
	m2 := New(a, nil, config.Config{})
	m2.running = true
	m2.ch = make(chan tea.Msg, 1)
	mm2, cmd := m2.Update(runDone{result: []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		messages.Human("hi"),
		messages.AI("done"),
	}})
	m2 = mm2.(*Model)
	if m2.running {
		t.Fatal("run must be finished")
	}
	if len(m2.history) != 2 || m2.history[0].Role != messages.RoleHuman {
		t.Fatalf("history after run = %+v", m2.history)
	}
	if cmd != nil {
		t.Fatal("no active goal: the continuation command must be nil")
	}
}

func TestObsBeginEnd(t *testing.T) {
	dir := t.TempDir()
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Obs:            obslog.New(filepath.Join(dir, "logs"), true),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without an agent (or obs) the run id is empty and obsEnd is a no-op.
	bare := New(nil, nil, config.Config{})
	if run := bare.obsBegin("p"); run != "" {
		t.Fatalf("obsBegin without an agent = %q, want empty", run)
	}
	bare.obsEnd("run", nil, nil)

	m := New(a, nil, config.Config{WorkDir: dir})
	run := m.obsBegin("prompt one")
	if run == "" {
		t.Fatal("obsBegin must return a run id when obs is wired")
	}
	// An empty run id short-circuits the end event.
	m.obsEnd("", nil, nil)
	// Both the success and the error tails log the run end and the dump.
	m.obsEnd(run, []messages.Message{messages.AI("x")}, nil)
	m.obsEnd(run, []messages.Message{messages.AI("x")}, errors.New("kaput"))

	// A settings store re-reads the debug-log switch before each run.
	st, err := settings.Open(filepath.Join(dir, "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m.settings = st
	if run2 := m.obsBegin("prompt two"); run2 == "" {
		t.Fatal("obsBegin with a settings store must still return a run id")
	}
}

func TestMaybeKickGoalRunsGoalLoop(t *testing.T) {
	a := newFakeAgent(t,
		messages.AI("assess 1"),
		messages.AI("work 1"),
		messages.AI(`{"verdict":"continue","gaps":"not done"}`),
	)
	if _, err := a.SetGoal("endless goal", "", 1, 1_000_000); err != nil {
		t.Fatal(err)
	}

	m := New(a, nil, config.Config{})
	// Guards: no agent, a busy session and a paused goal never kick.
	if cmd := New(nil, nil, config.Config{}).maybeKickGoal(); cmd != nil {
		t.Fatal("nil agent must not kick the goal loop")
	}
	m.running = true
	if cmd := m.maybeKickGoal(); cmd != nil {
		t.Fatal("a busy session must not kick the goal loop")
	}
	m.running = false
	if !a.PauseGoal() {
		t.Fatal("pause failed")
	}
	if cmd := m.maybeKickGoal(); cmd != nil {
		t.Fatal("a paused goal must not kick the loop")
	}
	if !a.ResumeGoal() {
		t.Fatal("resume failed")
	}

	cmd := m.maybeKickGoal()
	if cmd == nil {
		t.Fatal("an active idle goal must kick the loop")
	}
	if !m.running {
		t.Fatal("the goal loop must mark the session as running")
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "goal loop running") {
		t.Fatalf("transcript missing the goal loop banner:\n%s", strings.Join(m.lines, "\n"))
	}

	// Drain the goal run to completion; the single-cycle budget ends it.
	m = drainUntilDone(t, m, m.ch)
	if m.running {
		t.Fatal("goal loop did not finish")
	}
	if g := a.Goal(); g == nil || g.Status != agent.GoalBudgetLimited {
		t.Fatalf("goal status = %+v, want budget_limited", g)
	}
	// The exhausted goal is idle again: no re-kick.
	if cmd := m.maybeKickGoal(); cmd != nil {
		t.Fatal("an exhausted goal must not re-kick")
	}
}

func TestPendingAskUserInputEditing(t *testing.T) {
	q := agent.AskUserRequest{Questions: []tools.Question{{Question: "pick"}}}
	m, answerCh := newPendingModel(t, q)

	// Typing edits the answer line.
	m = press(m, key("a"))
	m = press(m, key("b"))
	if m.input != "ab" || m.cursor != 2 {
		t.Fatalf("typed answer = %q cursor=%d", m.input, m.cursor)
	}
	// Backspace deletes from the answer line.
	m = press(m, key("backspace"))
	if m.input != "a" || m.cursor != 1 {
		t.Fatalf("backspace answer = %q cursor=%d", m.input, m.cursor)
	}
	// Backspace on an empty line is a no-op.
	m = press(m, key("backspace"))
	if m.input != "" || m.cursor != 0 {
		t.Fatalf("empty backspace answer = %q cursor=%d", m.input, m.cursor)
	}
	// ctrl+c answers empty and quits.
	_, cmd := m.Update(ctrlKey("ctrl+c"))
	if !isQuitCmd(cmd) {
		t.Fatal("ctrl+c during a question must quit")
	}
	ans := <-answerCh
	aa, ok := ans.(agent.AskUserAnswer)
	if !ok || len(aa.Answers) != 0 {
		t.Fatalf("answer = %#v, want an empty AskUserAnswer", ans)
	}
}

func TestRendererEmitsGoalDeltas(t *testing.T) {
	m := New(nil, nil, config.Config{})
	ch := make(chan tea.Msg, 8)
	r := m.renderer(ch)
	r(graph.Step{Delta: agent.GoalDelta{Message: messages.Message{
		Role:      messages.RoleAI,
		ToolCalls: []messages.ToolCall{{ID: "g1", Name: "Edit", Args: map[string]any{"old_string": "a", "new_string": "b"}}},
	}}})
	if s := string((<-ch).(streamMsg)); !strings.Contains(s, "Edit") {
		t.Fatalf("goal delta tool chip = %q", s)
	}
	if s := string((<-ch).(streamMsg)); !strings.Contains(s, "- a") {
		t.Fatalf("goal delta diff del = %q", s)
	}
	if s := string((<-ch).(streamMsg)); !strings.Contains(s, "+ b") {
		t.Fatalf("goal delta diff add = %q", s)
	}
	if flight := (<-ch).(inFlightMsg); len(flight) != 1 || flight[0] != "Edit" {
		t.Fatalf("goal delta in-flight = %v", flight)
	}
}

func TestRunHeadlessFailsWithoutTTY(t *testing.T) {
	// Run wires the full program: options, version, and the bubbletea loop.
	// Without a TTY bubbletea cannot start its input reader, so a headless
	// run deterministically fails with that error (covering every Run
	// statement). Environments with a controlling terminal would instead wait
	// for real keypresses, so they skip.
	if _, err := os.Open("/dev/tty"); err == nil {
		t.Skip("controlling terminal present: Run would wait for real input")
	}
	a := newFakeAgent(t, messages.AI("ok"))
	cfg := config.Config{}
	c := console.New(console.Deps{Agent: a, Cfg: &cfg})
	st, err := settings.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := Run(a, nil, cfg, "v-test", WithConsole(c), WithSettingsStore(st)); err == nil {
		t.Fatal("Run must fail headless: no TTY available for the input reader")
	}
	// An empty version keeps the default.
	if err := Run(a, nil, cfg, "", WithConsole(c)); err == nil {
		t.Fatal("Run must fail headless: no TTY available for the input reader")
	}
}
