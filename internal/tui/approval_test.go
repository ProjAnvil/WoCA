package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEscape}
	}
	if s == "backspace" {
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newPendingModel(t *testing.T, value any) (*Model, chan any) {
	t.Helper()
	m := New(nil, nil, config.Config{})
	answerCh := make(chan any, 1)
	msg := interruptMsg{value: value, answerCh: answerCh}
	m.ch = make(chan tea.Msg, 1) // non-nil so Update keeps draining
	model, _ := m.Update(msg)
	m = model.(*Model)
	if m.pending == nil {
		t.Fatal("interrupt did not become pending")
	}
	return m, answerCh
}

func TestPendingPermissionApprove(t *testing.T) {
	m, answerCh := newPendingModel(t, agent.PermissionRequest{ToolName: "Bash", Reason: "runs a shell command"})
	if v := m.View(); !strings.Contains(v, "Allow Bash?") || !strings.Contains(v, "awaiting your decision") {
		t.Fatalf("view missing approval box:\n%s", v)
	}
	model, _ := m.Update(key("y"))
	m = model.(*Model)
	select {
	case ans := <-answerCh:
		if ans != "y" {
			t.Fatalf("answer = %v, want y", ans)
		}
	default:
		t.Fatal("no answer after approve key")
	}
	if m.pending != nil {
		t.Fatal("pending not cleared after approve")
	}
	if v := m.View(); strings.Contains(v, "awaiting your decision") {
		t.Fatal("approval box still rendered after resolution")
	}
	// The decision is echoed into the transcript.
	m.flush()
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "allowed by user") {
		t.Fatalf("transcript missing the decision echo:\n%s", joined)
	}
}

func TestPendingPermissionDenyViaN(t *testing.T) {
	m, answerCh := newPendingModel(t, agent.PermissionRequest{ToolName: "Write", Reason: "writes a file"})
	model, _ := m.Update(key("n"))
	m = model.(*Model)
	if ans := <-answerCh; ans != "n" {
		t.Fatalf("answer = %v, want n", ans)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "denied by user") {
		t.Fatal("transcript missing the denial echo")
	}
}

func TestPendingPermissionDenyViaEsc(t *testing.T) {
	m, answerCh := newPendingModel(t, agent.PermissionRequest{ToolName: "Bash", Reason: "x"})
	model, _ := m.Update(key("esc"))
	m = model.(*Model)
	if ans := <-answerCh; ans != "n" {
		t.Fatalf("answer = %v, want n", ans)
	}
}

func TestPendingPlanApproval(t *testing.T) {
	m, answerCh := newPendingModel(t, agent.PlanApprovalRequest{Plan: "do the thing"})
	if v := m.View(); !strings.Contains(v, "Approve this plan?") {
		t.Fatalf("view missing plan approval: %s", v)
	}
	// Enter executes the highlighted option, and the highlight defaults to
	// the safe choice (deny) — approving needs either the y shortcut or an
	// explicit move onto allow.
	model, _ := m.Update(key("enter"))
	_ = model.(*Model)
	if ans := <-answerCh; ans != "n" {
		t.Fatalf("enter must execute the deny default, got %v", ans)
	}
	m2, answerCh2 := newPendingModel(t, agent.PlanApprovalRequest{Plan: "do the thing"})
	mm2, _ := m2.Update(ctrlKey("right"))
	_ = mm2.(*Model)
	mm2b, _ := m2.Update(key("enter"))
	_ = mm2b.(*Model)
	if ans := <-answerCh2; ans != "y" {
		t.Fatalf("enter on the highlighted allow must approve, got %v", ans)
	}
}

func TestPendingAskUserQuestion(t *testing.T) {
	q := agent.AskUserRequest{Questions: []tools.Question{{Question: "pick one"}}}
	m, answerCh := newPendingModel(t, q)
	if v := m.View(); !strings.Contains(v, "pick one") {
		t.Fatalf("view missing the question: %s", v)
	}
	// Typing while pending edits the answer line, not a new prompt.
	for _, r := range []string{"o", "p", "t", "2"} {
		model, _ := m.Update(key(r))
		m = model.(*Model)
	}
	model, _ := m.Update(key("enter"))
	m = model.(*Model)
	ans := <-answerCh
	aa, ok := ans.(agent.AskUserAnswer)
	if !ok || len(aa.Answers) != 1 || aa.Answers[0] != "opt2" {
		t.Fatalf("answer = %+v, want [opt2]", ans)
	}
}

// TestRunInterruptApprovalEndToEnd drives a full run whose model calls Write
// in default mode: the TUI must surface the permission interrupt, the user
// approves with 'y', and the run resumes to completion with the tool
// executed exactly once.
func TestRunInterruptApprovalEndToEnd(t *testing.T) {
	dir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "out.txt", "content": "hi"}},
			}},
			messages.AI("written"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	m := New(a, nil, config.Config{})
	m.history = []messages.Message{messages.Human("write it")}
	m.running = true
	_ = m.run() // starts the run goroutine; messages flow into m.ch
	ch := m.ch

	// Drain run messages until the permission interrupt surfaces.
	var answerCh chan any
	deadline := time.After(5 * time.Second)
	for answerCh == nil {
		select {
		case msg := <-ch:
			switch v := msg.(type) {
			case interruptMsg:
				answerCh = v.answerCh
				mm, _ := m.Update(v)
				m = mm.(*Model)
			case runDone:
				t.Fatalf("run finished without an interrupt: err=%v", v.err)
			default:
				mm, _ := m.Update(msg)
				m = mm.(*Model)
			}
		case <-deadline:
			t.Fatal("interrupt never surfaced")
		}
	}
	if m.pending == nil {
		t.Fatal("interrupt not pending")
	}

	// Approve with 'y' and drain to completion.
	mm, _ := m.Update(key("y"))
	m = mm.(*Model)
	if m.pending != nil {
		t.Fatal("pending not cleared by the approve key")
	}
	// The run goroutine consumes the answer and resumes; completion plus the
	// tool's side effect are the authoritative assertions.
	for done := false; !done; {
		select {
		case msg := <-ch:
			if d, ok := msg.(runDone); ok {
				done = true
				if d.err != nil {
					t.Fatalf("run failed: %v", d.err)
				}
				if last := d.result[len(d.result)-1]; last.Content != "written" {
					t.Fatalf("final message = %+v", last)
				}
			} else {
				mm, _ := m.Update(msg)
				m = mm.(*Model)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("run did not finish after approval")
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("approved Write did not execute: %v %q", err, data)
	}
}
