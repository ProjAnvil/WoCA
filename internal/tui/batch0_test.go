package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

// awaitCanceled polls until the context's cancel takes effect (AfterFunc
// fires on its own goroutine, so a bare select/default would race).
func awaitCanceled(t *testing.T, canceled <-chan struct{}) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-canceled:
			return
		case <-deadline:
			t.Fatal("run context was not canceled")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestDoubleEscInterruptsRun wires a real cancelable context and verifies the
// double-esc window: one esc only arms, the second press inside the window
// cancels the run, and a slow second press does not.
func TestDoubleEscInterruptsRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan struct{})
	context.AfterFunc(ctx, func() { close(canceled) })

	m := New(nil, nil, config.Config{})
	m.running = true
	m.cancelRun = cancel

	// The first esc only arms (no cancel yet).
	m = press(m, key("esc"))
	select {
	case <-canceled:
		t.Fatal("a single esc must not cancel the run")
	default:
	}
	if m.interrupting {
		t.Fatal("a single esc must not mark an interrupt")
	}
	// The second esc inside the window cancels.
	m = press(m, key("esc"))
	if !m.interrupting {
		t.Fatal("double-esc must mark the interrupt")
	}
	awaitCanceled(t, canceled)
	// A stale lastEsc (outside the window) only re-arms.
	m2 := New(nil, nil, config.Config{})
	m2.running = true
	m2.cancelRun = cancel
	m2.lastEsc = m2.lastEsc.Add(-escDoubleWindow - time.Second)
	m2 = press(m2, key("esc"))
	if m2.interrupting {
		t.Fatal("a slow second esc must not interrupt")
	}
}

// TestInterruptedRunRendersMarkerNotError drives a runDone carrying a
// context.Canceled error and checks the interrupt marker (no error banner).
func TestInterruptedRunRendersMarkerNotError(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.running = true
	m.ch = make(chan tea.Msg, 1)
	m.interrupting = true
	mm, _ := m.Update(runDone{err: context.Canceled})
	m = mm.(*Model)
	m.flush()
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "interrupted by user") {
		t.Fatalf("transcript missing the interrupt marker:\n%s", joined)
	}
	if strings.Contains(joined, "Error:") {
		t.Fatalf("a user interrupt must not render as an error:\n%s", joined)
	}

	// A context.Canceled without the interrupting flag (canceled elsewhere)
	// still renders as the marker, not a failure.
	m2 := New(nil, nil, config.Config{})
	m2.running = true
	m2.ch = make(chan tea.Msg, 1)
	mm2, _ := m2.Update(runDone{err: fmt.Errorf("wrapped: %w", context.Canceled)})
	m2 = mm2.(*Model)
	m2.flush()
	if !strings.Contains(strings.Join(m2.lines, "\n"), "interrupted by user") {
		t.Fatalf("canceled-without-flag must still render the marker:\n%s", strings.Join(m2.lines, "\n"))
	}
}

// TestQueuedMessageFlushesWhenIdle parks a message during a run and checks
// the runDone flush: the parked line is dispatched as the next turn (a
// console command stays local; a prompt starts a run).
func TestQueuedMessageFlushesWhenIdle(t *testing.T) {
	// A parked console command runs locally on flush.
	m := newConsoleModel(t, messages.AI("ok"))
	m.running = true
	m.ch = make(chan tea.Msg, 1)
	m.queued = &queuedMsg{text: "/todos"}
	mm, _ := m.Update(runDone{result: []messages.Message{messages.AI("done")}})
	m = mm.(*Model)
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "no todos") {
		t.Fatalf("queued command was not flushed:\n%s", strings.Join(m.lines, "\n"))
	}
	if m.running {
		t.Fatal("a local command flush must leave the session idle")
	}

	// A parked prompt starts the next agent run.
	p := newConsoleModel(t, messages.AI("ok"))
	p.running = true
	p.ch = make(chan tea.Msg, 1)
	p.queued = &queuedMsg{text: "follow-up"}
	mm2, _ := p.Update(runDone{result: []messages.Message{messages.AI("done")}})
	p = mm2.(*Model)
	if !p.running {
		t.Fatal("the parked prompt must start a run on flush")
	}
	if len(p.history) == 0 || p.history[len(p.history)-1].Content != "follow-up" {
		t.Fatalf("history missing the flushed prompt: %+v", p.history)
	}
	p = drainUntilDone(t, p, p.ch)
	p.flush()
	if !strings.Contains(strings.Join(p.lines, "\n"), "❯ follow-up") {
		t.Fatalf("transcript missing the flushed prompt echo:\n%s", strings.Join(p.lines, "\n"))
	}
}

// TestQueueIsLastWriteWins parks two messages in a row; the slot keeps only
// the newest (web useMessageQueue parity).
func TestQueueIsLastWriteWins(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.running = true
	m.input = "first"
	m = press(m, key("enter"))
	m.input = "second"
	m = press(m, key("enter"))
	if m.queued == nil || m.queued.text != "second" {
		t.Fatalf("queue must keep the newest message: %+v", m.queued)
	}
	if v := m.View(); !strings.Contains(v, "next: second") {
		t.Fatalf("view missing the queue chip:\n%s", v)
	}
}

// TestDecisionBoxPreservesDraft types a draft, delivers an interrupt, and
// checks the draft is held while deciding and restored afterwards.
func TestDecisionBoxPreservesDraft(t *testing.T) {
	m, answerCh := newPendingModel(t, agent.PermissionRequest{ToolName: "Bash", Reason: "x"})
	// The interrupt arrived on a model that already had a draft typed.
	m.draftHeld = true
	m.draft = "half-typed prompt"
	if v := m.View(); !strings.Contains(v, "half-typed prompt") {
		t.Fatalf("view must show the held draft:\n%s", v)
	}
	if m.input != "" {
		t.Fatalf("decision keys must not touch the draft, got input=%q", m.input)
	}
	m = press(m, key("y"))
	if ans := <-answerCh; ans != "y" {
		t.Fatalf("answer = %v", ans)
	}
	if m.input != "half-typed prompt" || m.cursor != len("half-typed prompt") {
		t.Fatalf("draft not restored: input=%q cursor=%d", m.input, m.cursor)
	}
}

// TestAskUserQuestionWithOptions drives a two-question batch: the first with
// single-select options, the second multi-select; the answers arrive in
// order.
func TestAskUserQuestionWithOptions(t *testing.T) {
	q := agent.AskUserRequest{Questions: []tools.Question{
		{
			Question: "deploy where?",
			Options:  []tools.QuestionOption{{Label: "staging"}, {Label: "prod", Description: "careful"}},
		},
		{
			Question:    "which checks?",
			MultiSelect: true,
			Options:     []tools.QuestionOption{{Label: "lint"}, {Label: "test"}, {Label: "e2e"}},
		},
	}}
	m, answerCh := newPendingModel(t, q)

	// The first question renders its options; down + enter picks prod.
	if v := m.View(); !strings.Contains(v, "deploy where?") || !strings.Contains(v, "prod") {
		t.Fatalf("first question view:\n%s", v)
	}
	m = press(m, ctrlKey("down"))
	m = press(m, key("enter"))
	if m.pending == nil || m.askIdx != 1 {
		t.Fatalf("after the first answer: pending=%v idx=%d", m.pending != nil, m.askIdx)
	}

	// The second question is multi-select: space toggles lint and e2e.
	press(m, key(" ")) // toggle lint (highlighted)
	press(m, ctrlKey("down"))
	press(m, ctrlKey("down"))
	press(m, key(" ")) // toggle e2e
	m = press(m, key("enter"))
	ans := <-answerCh
	aa, ok := ans.(agent.AskUserAnswer)
	if !ok || len(aa.Answers) != 2 || aa.Answers[0] != "prod" || aa.Answers[1] != "lint, e2e" {
		t.Fatalf("answers = %+v, want [prod, 'lint, e2e']", ans)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "answered by user: prod · lint, e2e") {
		t.Fatalf("transcript missing the answer echo:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestAskUserQuestionEscSkips checks esc abort: answered-so-far survive and
// the remaining questions get empty answers.
func TestAskUserQuestionEscSkips(t *testing.T) {
	q := agent.AskUserRequest{Questions: []tools.Question{
		{Question: "a?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}}},
		{Question: "b?", Options: []tools.QuestionOption{{Label: "x"}, {Label: "y"}}},
	}}
	m, answerCh := newPendingModel(t, q)
	press(m, key("enter")) // answer "one" to the first
	press(m, key("esc"))   // skip the rest
	ans := <-answerCh
	aa, ok := ans.(agent.AskUserAnswer)
	if !ok || len(aa.Answers) != 2 || aa.Answers[0] != "one" || aa.Answers[1] != "" {
		t.Fatalf("answers = %+v, want [one, '']", ans)
	}
}

// TestAskUserQuestionMultiSelectEnterWithoutToggles submits the highlighted
// option when nothing is toggled (predictable single answer).
func TestAskUserQuestionMultiSelectEnterWithoutToggles(t *testing.T) {
	q := agent.AskUserRequest{Questions: []tools.Question{
		{Question: "pick", MultiSelect: true, Options: []tools.QuestionOption{{Label: "a"}, {Label: "b"}}},
	}}
	m, answerCh := newPendingModel(t, q)
	press(m, key("enter"))
	ans := <-answerCh
	aa, ok := ans.(agent.AskUserAnswer)
	if !ok || len(aa.Answers) != 1 || aa.Answers[0] != "a" {
		t.Fatalf("answers = %+v, want [a]", ans)
	}
}

// TestDispatchRoutesPlainPromptWithConsole locks the routing seam: with the
// console attached (the production wiring), a plain prompt must start a
// model turn, not trip the console's unknown-command error.
func TestDispatchRoutesPlainPromptWithConsole(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m.input = "hello world"
	m = press(m, key("enter"))
	if !m.running {
		m.flush()
		t.Fatalf("plain prompt must start a run, transcript=%v", m.lines)
	}
	m = drainUntilDone(t, m, m.ch)
	m.flush()
	joined := strings.Join(m.lines, "\n")
	if strings.Contains(joined, "unknown command") {
		t.Fatalf("plain prompt misrouted to the console:\n%s", joined)
	}
	if !strings.Contains(joined, "❯ hello world") {
		t.Fatalf("transcript missing the prompt echo:\n%s", joined)
	}
}

// TestDispatchRoutesShellLine checks `!command` lines: shell output lands in
// the transcript and the conversation carries the shared [shell] feed.
func TestDispatchRoutesShellLine(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m.input = "!echo shell-works"
	m = press(m, key("enter"))
	if m.running {
		t.Fatal("a shell line must not start an agent run")
	}
	m.flush()
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "shell-works") {
		t.Fatalf("transcript missing the shell output:\n%s", joined)
	}
	if len(m.history) != 1 || !strings.Contains(m.history[0].Content, "[shell]") || !strings.Contains(m.history[0].Content, "echo shell-works") {
		t.Fatalf("history missing the [shell] feed: %+v", m.history)
	}
}

// TestDispatchRoutesUnknownAndSkillSlashLines pins the slash-line fallback:
// an unknown /name echoes the console error, while a /name matching a loaded
// skill becomes a model turn (web parity).
func TestDispatchRoutesUnknownAndSkillSlashLines(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m.input = "/nosuch"
	m = press(m, key("enter"))
	if m.running {
		t.Fatal("an unknown command must not start a run")
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "unknown command /nosuch") {
		t.Fatalf("transcript missing the unknown-command error:\n%s", strings.Join(m.lines, "\n"))
	}

	// A skill line goes to the model.
	sk := newConsoleModel(t, messages.AI("ok"))
	sk.agent.SetSkills([]skills.Skill{{Name: "pdf", Description: "pdf tools"}})
	sk.input = "/pdf extract the invoice"
	sk = press(sk, key("enter"))
	if !sk.running {
		sk.flush()
		t.Fatalf("a skill line must start a run, transcript=%v", sk.lines)
	}
	if last := sk.history[len(sk.history)-1]; !strings.Contains(last.Content, "/pdf extract the invoice") {
		t.Fatalf("history missing the skill prompt: %+v", last)
	}
}
