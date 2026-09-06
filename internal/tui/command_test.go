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
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/tools"
)

// newFakeAgent builds a bypass-permission agent over a scripted fake model.
func newFakeAgent(t *testing.T, responses ...messages.Message) *agent.Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(responses...),
	)
	a, err := agent.New(agent.Config{
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

// newConsoleModel builds a TUI model with the console command layer attached.
func newConsoleModel(t *testing.T, responses ...messages.Message) *Model {
	t.Helper()
	a := newFakeAgent(t, responses...)
	cfg := config.Config{}
	c := console.New(console.Deps{Agent: a, Cfg: &cfg})
	m := New(a, nil, cfg)
	m.SetConsole(c)
	return m
}

// drainUntilDone pumps messages from a run channel into the model until the
// run completes (runDone), failing the test on timeout.
func drainUntilDone(t *testing.T, m *Model, ch <-chan tea.Msg) *Model {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return m
			}
			mm, _ := m.Update(msg)
			m = mm.(*Model)
			if _, done := msg.(runDone); done {
				return m
			}
		case <-deadline:
			t.Fatal("run did not finish in time")
		}
	}
}

func TestHandleCommandWithoutConsole(t *testing.T) {
	m := New(nil, nil, config.Config{})
	handled, cmd := m.handleCommand("/help")
	if !handled || cmd != nil {
		t.Fatalf("bare /help: handled=%v cmd=%v", handled, cmd)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "Commands:") {
		t.Fatalf("transcript missing the limited help:\n%s", strings.Join(m.lines, "\n"))
	}
	if handled, _ := m.handleCommand("hello"); handled {
		t.Fatal("plain prompts are not commands")
	}
}

func TestHandleCommandWithConsole(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))

	// "/" alone parses to an empty command.
	if handled, _ := m.handleCommand("/"); handled {
		t.Fatal(`handleCommand("/") must not claim handling`)
	}
	// Unknown commands surface their error in the transcript.
	if handled, _ := m.handleCommand("/nosuch"); !handled {
		t.Fatal("unknown command must be handled (error echoed)")
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "Error: unknown command /nosuch") {
		t.Fatalf("transcript missing the command error:\n%s", strings.Join(m.lines, "\n"))
	}
	// /clear wipes the transcript, the draft and the history.
	m.write("stuff\n")
	m.history = []messages.Message{messages.Human("old")}
	if handled, _ := m.handleCommand("/clear"); !handled {
		t.Fatal("/clear must be handled")
	}
	if len(m.lines) != 1 || m.lines[0] != "conversation cleared" || m.history != nil {
		t.Fatalf("after /clear: lines=%v history=%d", m.lines, len(m.history))
	}
	if len(m.viewport.Visible()) != 1 {
		t.Fatalf("viewport not reset: %v", m.viewport.Visible())
	}
	// /help prints the full catalog through the console.
	if handled, _ := m.handleCommand("/help"); !handled {
		t.Fatal("/help must be handled")
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "/theme") {
		t.Fatalf("transcript missing the console help:\n%s", strings.Join(m.lines, "\n"))
	}
}

func TestHandleCommandInitSubmitsPrompt(t *testing.T) {
	m := newConsoleModel(t, messages.AI("guide written"))
	handled, cmd := m.handleCommand("/init")
	if !handled || cmd == nil {
		t.Fatalf("/init: handled=%v cmd=%v", handled, cmd)
	}
	if !m.running {
		t.Fatal("the /init prompt must start a run")
	}
	m = drainUntilDone(t, m, m.ch)
	m.flush()
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "Analyzing the codebase") {
		t.Fatalf("transcript missing the /init output:\n%s", joined)
	}
	if !strings.Contains(joined, "❯ Please analyze this codebase") {
		t.Fatalf("transcript missing the auto-submitted prompt:\n%s", joined)
	}
}

func TestHistoryForRunExpandsFileReferencesWithoutChangingDisplayHistory(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("file context"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := New(nil, nil, config.Config{WorkDir: workspace})
	model.history = []messages.Message{messages.Human("read @note.txt")}

	runHistory, err := model.historyForRun()
	if err != nil {
		t.Fatal(err)
	}
	if got := model.history[len(model.history)-1].Content; got != "read @note.txt" {
		t.Fatalf("display history = %q, want original prompt", got)
	}
	if got := runHistory[len(runHistory)-1].Content; !strings.Contains(got, "file context") {
		t.Fatalf("run history omitted referenced file: %q", got)
	}
}

func TestInvalidFileReferenceKeepsDraftWithoutAppendingTranscript(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(filepath.Dir(workspace), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	model := New(nil, nil, config.Config{WorkDir: workspace})
	model.input = "read @../outside.txt"
	model.cursor = len(model.input)

	model = press(model, key("enter"))
	if model.input != "read @../outside.txt" || model.running {
		t.Fatalf("draft=%q running=%v", model.input, model.running)
	}
	if len(model.history) != 0 {
		t.Fatalf("invalid submission changed history=%d", len(model.history))
	}
	for _, block := range model.blocks {
		if block.kind == blockUser {
			t.Fatalf("invalid submission appended a user block: %#v", model.blocks)
		}
	}
}

func TestViewTodoPanel(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.todos = []tools.Todo{
		{Content: "alpha", Status: "in_progress", ActiveForm: "doing alpha"},
		{Content: "beta", Status: "completed"},
		{Content: "gamma", Status: "cancelled"},
		{Content: "delta", Status: "pending"},
	}
	v := m.View()
	for _, want := range []string{"Todo list", "[>]", "doing alpha", "[x] beta", "[-] gamma", "[ ] delta"} {
		if !strings.Contains(v, want) {
			t.Fatalf("todo panel missing %q:\n%s", want, v)
		}
	}
}

func TestViewFooterAndMenus(t *testing.T) {
	m := New(nil, nil, config.Config{Model: "cfg-model", PermissionMode: config.ModeDefault})
	// A running session with nothing in flight shows the thinking spinner.
	m.running = true
	if v := m.View(); !strings.Contains(v, "thinking") {
		t.Fatalf("running view missing the thinking spinner:\n%s", v)
	}
	// The command menu renders rows for a slash prefix.
	m.running = false
	m.input = "/"
	v := m.View()
	if !strings.Contains(v, "/help") || !strings.Contains(v, "/exit") {
		t.Fatalf("command menu missing entries:\n%s", v)
	}
	if !strings.Contains(v, "model=cfg-model") {
		t.Fatalf("footer missing the static model:\n%s", v)
	}
	// A live console configuration overrides the static one in the footer.
	a := newFakeAgent(t)
	cfg := config.Config{Model: "static"}
	c := console.New(console.Deps{Agent: a, Cfg: &cfg})
	cfg.Model = "live-model"
	m.SetConsole(c)
	if v := m.View(); !strings.Contains(v, "model=live-model") {
		t.Fatalf("footer missing the live model:\n%s", v)
	}
	// An active goal adds its segment to the footer.
	if _, err := a.SetGoal("ship it", "", 3, 1000); err != nil {
		t.Fatal(err)
	}
	m.agent = a
	if v := m.View(); !strings.Contains(v, "goal[active]") || !strings.Contains(v, "cycle 0/3") {
		t.Fatalf("footer missing the goal segment:\n%s", v)
	}
}
