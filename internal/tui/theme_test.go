package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/projanvil/woca/internal/config"
)

func resetUITest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = SetTheme("dark")
		SetVimMode(false)
	})
}

func TestThemeSwitch(t *testing.T) {
	resetUITest(t)
	if CurrentTheme() != "dark" {
		t.Fatalf("default theme = %s", CurrentTheme())
	}
	// Force a color profile so rendered output differs per palette even
	// without a TTY.
	lipgloss.SetColorProfile(termenv.ANSI256)
	before := promptStyle.Render("x")

	if err := SetTheme("light"); err != nil {
		t.Fatal(err)
	}
	if CurrentTheme() != "light" || promptStyle.Render("x") == before {
		t.Fatalf("theme switch did not restyle: %s", CurrentTheme())
	}
	if err := SetTheme("bogus"); err == nil {
		t.Fatal("unknown theme should fail")
	}
	if err := SetTheme("high-contrast"); err != nil {
		t.Fatal(err)
	}
	if CurrentTheme() != "high-contrast" {
		t.Fatalf("theme = %s", CurrentTheme())
	}
	names := ThemeNames()
	if len(names) != 3 {
		t.Fatalf("themes = %v", names)
	}
}

func TestVimNormalModeEditing(t *testing.T) {
	resetUITest(t)
	SetVimMode(true)
	m := New(nil, nil, config.Config{})
	m.input = "hello world"
	m.cursor = len(m.input)

	// b: word back, then x deletes the char under the cursor.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if m.cursor != 6 {
		t.Fatalf("b cursor = %d, want 6", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.input != "hello orld" {
		t.Fatalf("x input = %q", m.input)
	}

	// 0 and $ move to line edges; h/l step.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("0")})
	if m.cursor != 0 {
		t.Fatalf("0 cursor = %d", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if m.cursor != 1 {
		t.Fatalf("l cursor = %d", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	if m.cursor != len(m.input) {
		t.Fatalf("$ cursor = %d", m.cursor)
	}

	// dd clears the line.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if m.input != "" || m.cursor != 0 {
		t.Fatalf("dd input = %q cursor = %d", m.input, m.cursor)
	}

	// i enters insert mode; printable text appends again.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if m.input != "z" {
		t.Fatalf("insert input = %q", m.input)
	}
	// esc returns to normal mode; typing no longer appends.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.input != "z" {
		t.Fatalf("normal-mode q should not append: %q", m.input)
	}
}

func TestVimStatusIndicator(t *testing.T) {
	resetUITest(t)
	SetVimMode(true)
	m := New(nil, nil, config.Config{})
	if !strings.Contains(m.View(), "vim: NORMAL") {
		t.Fatalf("normal indicator missing:\n%s", m.View())
	}
	m.vimInsert = true
	if !strings.Contains(m.View(), "vim: INSERT") {
		t.Fatalf("insert indicator missing:\n%s", m.View())
	}
}

func TestVimOffKeepsPlainInput(t *testing.T) {
	resetUITest(t)
	SetVimMode(false)
	m := New(nil, nil, config.Config{})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.input != "x" {
		t.Fatalf("plain input broken: %q", m.input)
	}
}
