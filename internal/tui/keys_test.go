package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/tools"
)

// ctrlKey builds a non-rune key message by its canonical name (as reported by
// tea.KeyMsg.String(), which is what Update switches on).
func ctrlKey(s string) tea.KeyMsg {
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+j":
		return tea.KeyMsg{Type: tea.KeyCtrlJ}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press feeds one message through Update, returning the model.
func press(m *Model, msg tea.Msg) *Model {
	mm, _ := m.Update(msg)
	return mm.(*Model)
}

// isQuitCmd reports whether a command terminates the program.
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestQuitKeys(t *testing.T) {
	// ctrl+d always quits immediately.
	m := New(nil, nil, config.Config{})
	if _, cmd := m.Update(ctrlKey("ctrl+d")); !isQuitCmd(cmd) {
		t.Fatal("ctrl+d must quit the program")
	}
	// Idle ctrl+c with an empty draft arms the exit; the second press inside
	// the window quits.
	m = New(nil, nil, config.Config{})
	if _, cmd := m.Update(ctrlKey("ctrl+c")); isQuitCmd(cmd) {
		t.Fatal("the first idle ctrl+c must only arm the exit")
	}
	if _, cmd := m.Update(ctrlKey("ctrl+c")); !isQuitCmd(cmd) {
		t.Fatal("the second idle ctrl+c must quit the program")
	}
	// Idle ctrl+c with a draft clears the draft instead of arming the exit.
	m = New(nil, nil, config.Config{})
	m.input = "draft"
	m = press(m, ctrlKey("ctrl+c"))
	if m.input != "" {
		t.Fatalf("idle ctrl+c must clear the draft, got %q", m.input)
	}
	if _, cmd := m.Update(ctrlKey("ctrl+c")); isQuitCmd(cmd) {
		t.Fatal("the press that cleared the draft must not quit")
	}
	// While a turn runs, ctrl+c interrupts it (the cancel is wired through
	// a real context).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New(nil, nil, config.Config{})
	r.running = true
	r.cancelRun = cancel
	canceled := make(chan struct{})
	context.AfterFunc(ctx, func() { close(canceled) })
	r = press(r, ctrlKey("ctrl+c"))
	if !r.interrupting {
		t.Fatal("ctrl+c while running must mark the interrupt")
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-canceled:
			return
		case <-deadline:
			t.Fatal("ctrl+c while running must cancel the run context")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestWindowSizeResizesViewport(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m = press(m, tea.WindowSizeMsg{Width: 80, Height: 12})
	if m.height != 10 {
		t.Fatalf("height = %d, want 10 (window minus prompt box)", m.height)
	}
	// Tiny windows clamp instead of going negative.
	m = press(m, tea.WindowSizeMsg{Width: 80, Height: 2})
	if m.height != 1 {
		t.Fatalf("clamped height = %d, want 1", m.height)
	}
}

func TestEditingKeys(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.input = "abc"
	m.cursor = 1

	m = press(m, ctrlKey("ctrl+a"))
	if m.cursor != 0 {
		t.Fatalf("ctrl+a cursor = %d, want 0", m.cursor)
	}
	m = press(m, ctrlKey("ctrl+e"))
	if m.cursor != 3 {
		t.Fatalf("ctrl+e cursor = %d, want 3", m.cursor)
	}
	// left/right stop at the edges.
	m = press(m, ctrlKey("left"))
	if m.cursor != 2 {
		t.Fatalf("left cursor = %d, want 2", m.cursor)
	}
	m = press(m, ctrlKey("right"))
	if m.cursor != 3 {
		t.Fatalf("right cursor = %d, want 3", m.cursor)
	}
	m = press(m, ctrlKey("right"))
	if m.cursor != 3 {
		t.Fatalf("right past end moved the cursor to %d", m.cursor)
	}
	m.cursor = 0
	m = press(m, ctrlKey("left"))
	if m.cursor != 0 {
		t.Fatalf("left at start moved the cursor to %d", m.cursor)
	}
	// ctrl+u wipes the line.
	m.cursor = 2
	m.sel = 4
	m = press(m, ctrlKey("ctrl+u"))
	if m.input != "" || m.cursor != 0 || m.sel != 0 {
		t.Fatalf("ctrl+u input=%q cursor=%d sel=%d", m.input, m.cursor, m.sel)
	}
	// delete removes the character under the cursor.
	m.input = "abc"
	m.cursor = 1
	m = press(m, ctrlKey("delete"))
	if m.input != "ac" {
		t.Fatalf("delete input = %q, want ac", m.input)
	}

	// Editing keys keep working while a run is in flight: the composer stays
	// live and enter steers the run immediately instead of swallowing it.
	r := New(nil, nil, config.Config{})
	r.running = true
	r.input = "abc"
	r.cursor = 1
	for _, k := range []string{"right", "backspace", "ctrl+u"} {
		r = press(r, ctrlKey(k))
	}
	if r.input != "" || r.cursor != 0 {
		t.Fatalf("editing during a run changed input=%q cursor=%d", r.input, r.cursor)
	}
	r.input = "next message"
	r = press(r, key("enter"))
	if len(r.pendingSteers) != 1 || r.pendingSteers[0] != "next message" || r.input != "" {
		t.Fatalf("enter during a run must steer immediately: pendingSteers=%+v input=%q", r.pendingSteers, r.input)
	}
	// A steer is already committed, so ↑ on an empty line with no history
	// leaves it in place rather than recalling it for editing.
	r = press(r, ctrlKey("up"))
	if len(r.pendingSteers) != 1 || r.input != "" {
		t.Fatalf("up must not retract a committed steer: pendingSteers=%+v input=%q", r.pendingSteers, r.input)
	}
}

func TestCtrlJInsertsNewlineWithoutDispatching(t *testing.T) {
	model := New(nil, nil, config.Config{})
	model.input = "firstsecond"
	model.cursor = len("first")

	model = press(model, ctrlKey("ctrl+j"))
	if model.input != "first\nsecond" || model.cursor != len("first\n") || model.running {
		t.Fatalf("input=%q cursor=%d running=%v", model.input, model.cursor, model.running)
	}
	if got := model.renderInput(); !strings.Contains(got, "\n") {
		t.Fatalf("multiline input was not rendered: %q", got)
	}
}

func TestEditorResultReplacesDraftOnSuccess(t *testing.T) {
	model := New(nil, nil, config.Config{})
	model.input = "old draft"
	model.cursor = len(model.input)

	model.applyEditorResult(editorResult{content: "first line\nsecond line"})
	if model.input != "first line\nsecond line" || model.cursor != len(model.input) {
		t.Fatalf("input=%q cursor=%d", model.input, model.cursor)
	}
}

func TestEditorResultKeepsDraftOnFailure(t *testing.T) {
	model := New(nil, nil, config.Config{})
	model.input = "keep me"
	model.cursor = len(model.input)

	model.applyEditorResult(editorResult{err: fmt.Errorf("editor failed")})
	if model.input != "keep me" || model.cursor != len(model.input) || !strings.Contains(model.hint, "editor failed") {
		t.Fatalf("input=%q cursor=%d hint=%q", model.input, model.cursor, model.hint)
	}
}

func TestPageKeysScrollTranscript(t *testing.T) {
	m := New(nil, nil, config.Config{})
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	m.write(b.String())
	m.viewport.SetHeight(3)

	m = press(m, ctrlKey("pgdown"))
	if !m.viewport.AtBottom() {
		t.Fatal("pgdown should reach the bottom")
	}
	m = press(m, ctrlKey("pgup"))
	if m.viewport.AtBottom() {
		t.Fatal("pgup should scroll away from the bottom")
	}
}

func TestHistoryNavigationUpDown(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.inputHist = []string{"first", "second"}
	m.histIdx = 2

	m = press(m, ctrlKey("up"))
	if m.input != "second" || m.histIdx != 1 || m.cursor != len("second") {
		t.Fatalf("up: input=%q idx=%d cursor=%d", m.input, m.histIdx, m.cursor)
	}
	m = press(m, ctrlKey("up"))
	if m.input != "first" || m.histIdx != 0 {
		t.Fatalf("up: input=%q idx=%d", m.input, m.histIdx)
	}
	m = press(m, ctrlKey("down"))
	if m.input != "second" || m.histIdx != 1 {
		t.Fatalf("down: input=%q idx=%d", m.input, m.histIdx)
	}
	// Down past the newest entry clears the draft line.
	m = press(m, ctrlKey("down"))
	if m.input != "" || m.histIdx != 2 || m.cursor != 0 {
		t.Fatalf("down past end: input=%q idx=%d", m.input, m.histIdx)
	}

	// Arrows keep working while a run is in flight (history navigation and
	// menu movement stay live with the composer).
	r := New(nil, nil, config.Config{})
	r.running = true
	r.inputHist = []string{"one"}
	r.histIdx = 1
	r = press(r, ctrlKey("up"))
	if r.input != "one" {
		t.Fatalf("up during a run must browse history, got %q", r.input)
	}
	r = press(r, ctrlKey("down"))
	if r.input != "" {
		t.Fatalf("down during a run must return to the empty draft, got %q", r.input)
	}
}

func TestUpDownClampSuggestionSelection(t *testing.T) {
	n := len(suggestions("/"))
	if n < 2 {
		t.Fatalf("expected a command catalog, got %d", n)
	}
	m := New(nil, nil, config.Config{})
	m.input = "/"
	m.sel = n + 5 // far out of range
	m = press(m, ctrlKey("up"))
	if m.sel != n-2 {
		t.Fatalf("up clamp sel = %d, want %d", m.sel, n-2)
	}
	m.sel = n + 5
	m = press(m, ctrlKey("down"))
	if m.sel != n-1 {
		t.Fatalf("down clamp sel = %d, want %d", m.sel, n-1)
	}
}

func TestAtSuggestionsIncludeWorkspacePaths(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	model := New(nil, nil, config.Config{WorkDir: workspace})

	suggestions := model.allSuggestions("@doc")
	if len(suggestions) != 1 || suggestions[0].name != "@docs/" || suggestions[0].group != groupFiles {
		t.Fatalf("suggestions = %#v", suggestions)
	}
}

func TestTabCompletionClampsSelection(t *testing.T) {
	cmds := suggestions("/")
	m := New(nil, nil, config.Config{})
	m.input = "/"
	m.sel = 999
	m = press(m, ctrlKey("tab"))
	if m.input != cmds[len(cmds)-1].name {
		t.Fatalf("tab completed to %q, want the clamped last entry %q", m.input, cmds[len(cmds)-1].name)
	}
	if m.sel != 0 {
		t.Fatalf("tab must reset the selection, got %d", m.sel)
	}
	// Tab without a slash prefix does nothing.
	m.input = "plain"
	m = press(m, ctrlKey("tab"))
	if m.input != "plain" {
		t.Fatalf("tab completed a plain prompt: %q", m.input)
	}
	// Tab keeps completing while a run is in flight (the composer stays
	// live; enter parks instead of submitting).
	m.running = true
	m.input = "/ag"
	m = press(m, ctrlKey("tab"))
	if m.input != "/agents" {
		t.Fatalf("tab during a run completed to %q, want /agents", m.input)
	}
}

func TestEnterKeyFlow(t *testing.T) {
	// The command menu completes the highlighted command on the first enter.
	m := New(nil, nil, config.Config{})
	m.input = "/he"
	m.sel = 0
	m = press(m, key("enter"))
	if m.input != "/help" {
		t.Fatalf("enter completed to %q, want /help", m.input)
	}
	// An out-of-range selection clamps to the last matching entry.
	c := New(nil, nil, config.Config{})
	c.input = "/he"
	c.sel = 99
	c = press(c, key("enter"))
	if c.input != "/help" {
		t.Fatalf("enter clamped completion = %q, want /help", c.input)
	}
	// The second enter runs it (without a console only the short help works).
	m = press(m, key("enter"))
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "Commands:") {
		t.Fatalf("transcript missing the help text:\n%s", strings.Join(m.lines, "\n"))
	}

	// Blank input is a no-op and never recorded in history.
	b := New(nil, nil, config.Config{})
	b.input = "   "
	b = press(b, key("enter"))
	if b.input != "" || len(b.inputHist) != 0 {
		t.Fatalf("blank submit input=%q hist=%v", b.input, b.inputHist)
	}

	// /exit and /quit quit the program.
	for _, cmd := range []string{"/exit", "/quit"} {
		q := New(nil, nil, config.Config{})
		q.input = cmd
		if _, c := q.Update(key("enter")); !isQuitCmd(c) {
			t.Fatalf("%s must quit the program", cmd)
		}
	}

	// Enter while running steers immediately (no command is produced).
	r := New(nil, nil, config.Config{})
	r.running = true
	r.input = "queued"
	if _, c := r.Update(key("enter")); c != nil {
		t.Fatal("enter during a run must not produce a command")
	}
	if len(r.pendingSteers) != 1 || r.pendingSteers[0] != "queued" || r.input != "" {
		t.Fatalf("steer: pendingSteers=%+v input=%q", r.pendingSteers, r.input)
	}
}

func TestEnterSubmitsAndDedupsHistory(t *testing.T) {
	a := newFakeAgent(t, messages.AI("ok"))
	m := New(a, nil, config.Config{})
	m.inputHist = []string{"hi"}
	m.histIdx = 1

	m.input = "hi"
	m = press(m, key("enter"))
	if !m.running {
		t.Fatal("submit must mark the session as running")
	}
	if len(m.inputHist) != 1 {
		t.Fatalf("duplicate prompt re-recorded: %v", m.inputHist)
	}
	m = drainUntilDone(t, m, m.ch)
	if m.running {
		t.Fatal("run did not finish")
	}

	m.input = "again"
	m = press(m, key("enter"))
	if len(m.inputHist) != 2 || m.inputHist[1] != "again" {
		t.Fatalf("new prompt not recorded: %v", m.inputHist)
	}
	m = drainUntilDone(t, m, m.ch)
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "❯ again") {
		t.Fatalf("transcript missing the submitted prompt:\n%s", strings.Join(m.lines, "\n"))
	}
}

func TestVimNormalModeMotionsAndEdits(t *testing.T) {
	resetUITest(t)
	SetVimMode(true)
	m := New(nil, nil, config.Config{})
	m.input = "hi there"
	m.cursor = 0

	// w jumps to the next word; a enters insert mode after the cursor.
	m = press(m, key("w"))
	if m.cursor != 3 {
		t.Fatalf("w cursor = %d, want 3", m.cursor)
	}
	m = press(m, key("a"))
	if !m.vimInsert || m.cursor != 4 {
		t.Fatalf("a: insert=%v cursor=%d", m.vimInsert, m.cursor)
	}
	m = press(m, key("esc"))
	// A enters insert mode at the end of the line.
	m = press(m, key("A"))
	if !m.vimInsert || m.cursor != len(m.input) {
		t.Fatalf("A: insert=%v cursor=%d", m.vimInsert, m.cursor)
	}
	m = press(m, key("esc"))
	// h/l step by one character.
	m.cursor = 2
	m = press(m, key("h"))
	if m.cursor != 1 {
		t.Fatalf("h cursor = %d, want 1", m.cursor)
	}
	m = press(m, key("l"))
	if m.cursor != 2 {
		t.Fatalf("l cursor = %d, want 2", m.cursor)
	}
	// D truncates from the cursor on.
	m = press(m, key("D"))
	if m.input != "hi" {
		t.Fatalf("D input = %q, want hi", m.input)
	}
	// A pending d is cancelled by any other key (here: l at the end).
	m = press(m, key("d"))
	m = press(m, key("l"))
	if m.input != "hi" {
		t.Fatalf("cancelled dd changed the input to %q", m.input)
	}
	// dd clears the line.
	m = press(m, key("d"))
	m = press(m, key("d"))
	if m.input != "" || m.cursor != 0 {
		t.Fatalf("dd input = %q cursor = %d", m.input, m.cursor)
	}
	// Backspace moves left without deleting; delete removes under the cursor.
	m.input = "abc"
	m.cursor = 3
	m = press(m, key("backspace"))
	if m.cursor != 2 || m.input != "abc" {
		t.Fatalf("normal-mode backspace cursor=%d input=%q", m.cursor, m.input)
	}
	m = press(m, key("delete"))
	if m.input != "ab" {
		t.Fatalf("normal-mode delete input = %q, want ab", m.input)
	}
	// Esc in normal mode and unknown runes are consumed silently.
	m = press(m, key("esc"))
	m = press(m, key("z"))
	if m.input != "ab" {
		t.Fatalf("unknown rune typed into normal mode: %q", m.input)
	}
	// Multi-rune combos are not consumed by vimKey and fall through to the
	// standard handling (they type).
	m = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zz")})
	if m.input != "abzz" {
		t.Fatalf("multi-rune fallthrough input = %q, want abzz", m.input)
	}
	// b crosses leading whitespace backwards to the previous word start.
	m.input = "ab  cd"
	m.cursor = 4
	m = press(m, key("b"))
	if m.cursor != 0 {
		t.Fatalf("b cursor = %d, want 0", m.cursor)
	}
}

func TestVimWordMotions(t *testing.T) {
	if got := vimWordForward("hi there", 0); got != 3 {
		t.Fatalf("vimWordForward = %d, want 3", got)
	}
	if got := vimWordForward("hi", 2); got != 2 {
		t.Fatalf("vimWordForward at end = %d, want 2", got)
	}
	if got := vimWordForward("   x", 0); got != 3 {
		t.Fatalf("vimWordForward over leading spaces = %d, want 3", got)
	}
	if got := vimWordBackward("ab  cd", 4); got != 0 {
		t.Fatalf("vimWordBackward = %d, want 0", got)
	}
	if got := vimWordBackward("ab", 2); got != 0 {
		t.Fatalf("vimWordBackward = %d, want 0", got)
	}
}

func TestViewportEdges(t *testing.T) {
	// SetHeight clamps tiny heights to 1.
	v := NewViewport()
	v.SetHeight(0)
	if v.height != 1 {
		t.Fatalf("height = %d, want 1", v.height)
	}
	// A shrinking viewport clamps the offset.
	big := NewViewport()
	big.SetHeight(5)
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i)
	}
	big.SetLines(lines)
	big.ScrollToBottom() // offset 5
	big.SetHeight(8)     // maxOffset drops to 2
	if big.offset != 2 {
		t.Fatalf("offset = %d, want clamped 2", big.offset)
	}
	// SetLines clamps an offset that no longer exists.
	small := NewViewport()
	small.SetHeight(2)
	small.SetLines(lines)
	small.ScrollToBottom() // offset 8
	small.SetLines([]string{"a", "b", "c"})
	if small.offset != 1 {
		t.Fatalf("offset = %d, want clamped 1", small.offset)
	}
	// Visible never reads past the end of the content.
	over := NewViewport()
	over.SetHeight(8)
	over.SetLines(lines)
	over.offset = 7
	if got := over.Visible(); len(got) != 3 || got[0] != "l7" {
		t.Fatalf("visible = %v, want the last three lines", got)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := truncate(strings.Repeat("x", 10), 4); got != "xxxx…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("ab", 4); got != "ab" {
		t.Fatalf("truncate short = %q", got)
	}
	if got := padRight("ab", 5); got != "ab   " {
		t.Fatalf("padRight = %q", got)
	}
	if got := padRight("abcdef", 3); got != "abcdef" {
		t.Fatalf("padRight over-wide = %q", got)
	}
	long := summarize(strings.Repeat("y", 300))
	if long != strings.Repeat("y", 200)+"…" {
		t.Fatalf("summarize long = %d chars", len(long))
	}
}

func TestStripSystem(t *testing.T) {
	msgs := []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		messages.Human("hi"),
	}
	if got := stripSystem(msgs); len(got) != 1 || got[0].Role != messages.RoleHuman {
		t.Fatalf("stripSystem = %+v", got)
	}
	plain := []messages.Message{messages.Human("hi")}
	if got := stripSystem(plain); len(got) != 1 {
		t.Fatalf("stripSystem without a system prompt = %+v", got)
	}
}

func TestToolColorFullCatalog(t *testing.T) {
	for _, name := range []string{
		"Bash", "Read", "Glob", "Grep", "Edit", "Write", "NotebookEdit",
		"TodoWrite", "Task", "TaskCreate", "TaskUpdate", "WebFetch", "WebSearch",
	} {
		_ = toolColor(name) // must not panic
	}
}

func TestGoalFooter(t *testing.T) {
	g := &agent.Goal{Cycles: 1, MaxCycles: 5, TokensUsed: 1500, MaxTokens: 2000}
	if got := goalFooter(g); !strings.Contains(got, "cycle 1/5") {
		t.Fatalf("goalFooter = %q", got)
	}
	g.Claimed = true
	if got := goalFooter(g); !strings.Contains(got, "claim pending verification") {
		t.Fatalf("goalFooter claim = %q", got)
	}
}

func TestReadCmd(t *testing.T) {
	// A closed channel ends the read loop with a nil message.
	ch := make(chan tea.Msg)
	close(ch)
	if msg := readCmd(ch)(); msg != nil {
		t.Fatalf("readCmd on closed channel = %v, want nil", msg)
	}
	// An open channel yields the next message.
	open := make(chan tea.Msg, 1)
	open <- streamMsg("x")
	if msg := readCmd(open)(); msg != streamMsg("x") {
		t.Fatalf("readCmd = %v", msg)
	}
}

func TestAppendTrimsTranscript(t *testing.T) {
	m := New(nil, nil, config.Config{})
	total := transcriptLimit + 2
	for i := 0; i < total; i++ {
		m.append(fmt.Sprintf("l%d", i))
	}
	if len(m.lines) != transcriptLimit || m.lines[0] != "l2" || m.lines[transcriptLimit-1] != fmt.Sprintf("l%d", total-1) {
		t.Fatalf("append trim: len=%d first=%q last=%q", len(m.lines), m.lines[0], m.lines[len(m.lines)-1])
	}
}

func TestResolvePendingWithoutPendingInterrupt(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.resolvePending("y") // must be a safe no-op
	if m.pending != nil {
		t.Fatal("resolvePending must not create a pending interrupt")
	}
}

func TestPendingPromptDefaultAndMultiQuestion(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.pending = &interruptMsg{value: 42}
	if got := m.pendingPrompt(); got != "Continue?" {
		t.Fatalf("unknown interrupt prompt = %q", got)
	}
	// AskUserQuestion renders one question at a time; with a direct
	// pending assignment the counter starts at the first question.
	m.pending = &interruptMsg{value: agent.AskUserRequest{Questions: []tools.Question{
		{Question: "a"}, {Question: "b"},
	}}}
	m.pendingBegin()
	if got := m.pendingPrompt(); !strings.Contains(got, "(1/2) a") {
		t.Fatalf("multi-question prompt = %q", got)
	}
}

// TestShiftTabCyclesPermissionMode drives the composer parity binding: each
// press advances config.PermissionMode through the shared cycle order.
func TestShiftTabCyclesPermissionMode(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	// newConsoleModel's fixture starts the agent in ModeBypass without
	// syncing it to the console's cfg; normalize both first so the cycle
	// below is deterministic.
	if _, err := m.console.Execute(context.Background(), "permissions", []string{"default"}, &console.Session{}); err != nil {
		t.Fatal(err)
	}

	m = press(m, ctrlKey("shift+tab"))
	if got := m.console.Cfg().PermissionMode; got != config.ModeAcceptEdits {
		t.Fatalf("mode after one shift+tab = %s, want acceptEdits", got)
	}
	m = press(m, ctrlKey("shift+tab"))
	if got := m.console.Cfg().PermissionMode; got != config.ModePlan {
		t.Fatalf("mode after two shift+tab = %s, want plan", got)
	}
	if !strings.Contains(m.hint, "plan") {
		t.Fatalf("hint missing the new mode: %q", m.hint)
	}
}

// TestShiftTabIgnoredDuringCompletionMenu leaves the mode untouched while a
// "/" or "@" completion menu is open, matching the web composer's guard.
func TestShiftTabIgnoredDuringCompletionMenu(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	if _, err := m.console.Execute(context.Background(), "permissions", []string{"default"}, &console.Session{}); err != nil {
		t.Fatal(err)
	}
	m.input = "/"
	m = press(m, ctrlKey("shift+tab"))
	if got := m.console.Cfg().PermissionMode; got != config.ModeDefault {
		t.Fatalf("mode changed to %s while a completion menu was open", got)
	}
}
