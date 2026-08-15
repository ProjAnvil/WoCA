package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"

	"github.com/projanvil/woca/internal/graph"
)

func TestModelWriteAccumulatesLines(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.write("hello ")
	m.write("wor")
	m.write("ld\nnext")
	m.flush()
	if len(m.lines) != 2 || m.lines[0] != "hello world" || m.lines[1] != "next" {
		t.Fatalf("lines = %+v", m.lines)
	}
}

func TestRendererEmitsDeltasAndTools(t *testing.T) {
	m := New(nil, nil, config.Config{})
	ch := make(chan tea.Msg, 8)
	r := m.renderer(ch)
	r(graph.Step{Delta: "hello "})
	r(graph.Step{Delta: "world"})
	r(graph.Step{Update: graph.State{"messages": []messages.Message{
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{Name: "Bash", Args: map[string]any{"command": "ls"}}}},
	}}})

	got := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		msg := <-ch
		got = append(got, string(msg.(streamMsg)))
	}
	if got[0] != "hello " || got[1] != "world" {
		t.Fatalf("deltas = %q", got[:2])
	}
	if !strings.Contains(got[2], "Bash") {
		t.Fatalf("tool line = %q", got[2])
	}
}

func TestSummarize(t *testing.T) {
	if summarize("  a\nb  ") != "a" {
		t.Fatalf("summarize = %q", summarize("  a\nb  "))
	}
	if summarize("") != "(no output)" {
		t.Fatalf("summarize empty = %q", summarize(""))
	}
}

func TestViewContainsPromptBox(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.write("hello")
	v := m.View()
	if !strings.Contains(v, "❯") {
		t.Fatalf("view missing ❯ prompt: %q", v)
	}
	if !strings.Contains(v, "╭") {
		t.Fatalf("view missing rounded box border: %q", v)
	}
}

func TestToolColorNoPanic(t *testing.T) {
	for _, name := range []string{"Bash", "Read", "Edit", "TodoWrite", "Unknown"} {
		_ = toolColor(name) // must not panic
	}
}

func TestViewShowsBannerWhenEmpty(t *testing.T) {
	m := New(nil, nil, config.Config{})
	v := m.View()
	if !strings.Contains(v, "Welcome to woca") {
		t.Fatalf("banner missing: %q", v)
	}
}

func TestDiffLines(t *testing.T) {
	lines := diffLines(map[string]any{"old_string": "foo", "new_string": "bar"})
	if len(lines) != 2 {
		t.Fatalf("diff lines = %v", lines)
	}
	if !strings.Contains(lines[0], "foo") || !strings.Contains(lines[1], "bar") {
		t.Fatalf("diff lines = %v", lines)
	}
}

func TestViewShowsSpinnerTree(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.running = true
	m.inFlight = []string{"Bash", "Read"}
	v := m.View()
	if !strings.Contains(v, "Bash") || !strings.Contains(v, "Read") {
		t.Fatalf("spinner tree missing: %q", v)
	}
	idle := New(nil, nil, config.Config{})
	if strings.Contains(idle.View(), "thinking") {
		t.Fatalf("idle view should not show a spinner: %q", idle.View())
	}
}

func TestRendererEmitsInFlight(t *testing.T) {
	m := New(nil, nil, config.Config{})
	ch := make(chan tea.Msg, 8)
	r := m.renderer(ch)
	r(graph.Step{Update: graph.State{"messages": []messages.Message{
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "1", Name: "Bash", Args: map[string]any{"command": "ls"}}}},
	}}})
	<-ch // tool chip
	msg := <-ch
	flight, ok := msg.(inFlightMsg)
	if !ok || len(flight) != 1 || flight[0] != "Bash" {
		t.Fatalf("inFlight = %#v", msg)
	}
}

func TestSuggestions(t *testing.T) {
	if got := suggestions("/"); len(got) != len(slashCommands) {
		t.Fatalf("suggestions(/) = %v, want all %d", got, len(slashCommands))
	}
	if got := suggestions("/h"); len(got) != 1 || got[0].name != "/help" {
		t.Fatalf("suggestions(/h) = %v", got)
	}
	if got := suggestions("hi"); got != nil {
		t.Fatalf("suggestions(hi) = %v, want nil", got)
	}
	pms := suggestions("/p")
	if len(pms) < 1 || pms[0].name != "/permissions" || !containsCmd(pms, "/plugins") {
		t.Fatalf("suggestions(/p) = %v", pms)
	}
}

func TestArrowKeysDoNotAppendText(t *testing.T) {
	m := New(nil, nil, config.Config{})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.input != "" {
		t.Fatalf("arrow keys must not append text, got %q", m.input)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', 'b'}})
	if m.input != "ab" {
		t.Fatalf("typed runes should append, got %q", m.input)
	}
}

func TestRenderInputCursor(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.input = "ab"
	m.cursor = 1
	got := m.renderInput()
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Fatalf("renderInput = %q", got)
	}
}

func TestCursorMovementAndBackspace(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.input = "abc"
	m.cursor = 3
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.cursor != 2 {
		t.Fatalf("left cursor = %d, want 2", m.cursor)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.input != "ac" || m.cursor != 1 {
		t.Fatalf("backspace input=%q cursor=%d", m.input, m.cursor)
	}
}

func TestCommandPaletteNavigationClamps(t *testing.T) {
	m := New(nil, nil, config.Config{})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.sel != 0 {
		t.Fatalf("initial sel = %d", m.sel)
	}
	// Down only moves the highlight; it does not change the input.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	cmds := suggestions("/")
	if len(cmds) < 2 {
		t.Fatalf("expected a command catalog, got %d", len(cmds))
	}
	if m.sel != 1 || m.input != "/" {
		t.Fatalf("after down: sel=%d input=%q", m.sel, m.input)
	}
	// Tab completes the highlighted command.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.input != cmds[1].name {
		t.Fatalf("tab input = %q, want %s", m.input, cmds[1].name)
	}
	// Down again must not panic: the list narrowed to one entry, so sel clamps.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.input != cmds[1].name {
		t.Fatalf("down input = %q", m.input)
	}
}

func containsCmd(list []command, name string) bool {
	for _, c := range list {
		if c.name == name {
			return true
		}
	}
	return false
}
