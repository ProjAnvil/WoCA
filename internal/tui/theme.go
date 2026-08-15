package tui

import (
	"fmt"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// Themes (a small port of Claude Code's /theme): named palettes that
// re-style the transcript. SetTheme reassigns the package styles; it runs on
// the bubbletea goroutine (the /theme command), so no locking is needed
// beyond the mutex guarding concurrent tests.

type Theme struct {
	Name        string
	Description string
	// ANSI 256-color numbers per role.
	Prompt, User, Dim, Accent, Border, Sel, SelText string
}

var themes = []Theme{
	{Name: "dark", Description: "dark default (cyan prompt, orange accents)",
		Prompt: "6", User: "15", Dim: "8", Accent: "208", Border: "8", Sel: "5", SelText: "15"},
	{Name: "light", Description: "light palettes for bright terminals",
		Prompt: "4", User: "0", Dim: "7", Accent: "18", Border: "7", Sel: "6", SelText: "15"},
	{Name: "high-contrast", Description: "maximum contrast (accessibility)",
		Prompt: "14", User: "15", Dim: "7", Accent: "11", Border: "15", Sel: "12", SelText: "0"},
}

var (
	themeMu       sync.Mutex
	currentTheme  = "dark"
	vimEnabledMux sync.Mutex
	vimEnabled    bool
)

// ThemeNames lists available theme names.
func ThemeNames() []string {
	out := make([]string, 0, len(themes))
	for _, t := range themes {
		out = append(out, t.Name)
	}
	return out
}

// CurrentTheme returns the active theme name.
func CurrentTheme() string {
	themeMu.Lock()
	defer themeMu.Unlock()
	return currentTheme
}

// SetTheme switches the palette by name.
func SetTheme(name string) error {
	for _, t := range themes {
		if t.Name == name {
			themeMu.Lock()
			currentTheme = name
			applyThemeStyles(t)
			themeMu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("unknown theme %q (available: %v)", name, ThemeNames())
}

// applyThemeStyles rebuilds the package style vars from a palette. Caller
// holds themeMu.
func applyThemeStyles(t Theme) {
	dimStyle = lipgloss.NewStyle().Faint(true)
	boldDimStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	promptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.Prompt))
	userStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.User))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	boxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(t.Border))
	addStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	delStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	cursorStyle = lipgloss.NewStyle().Reverse(true)
	selStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(t.Sel)).Foreground(lipgloss.Color(t.SelText))
	selDescStyle = lipgloss.NewStyle().Background(lipgloss.Color(t.Sel)).Foreground(lipgloss.Color(t.SelText)).Faint(true)
}

// SetVimMode toggles modal (vim) input editing.
func SetVimMode(on bool) {
	vimEnabledMux.Lock()
	vimEnabled = on
	vimEnabledMux.Unlock()
}

// VimMode reports whether vim input mode is on.
func VimMode() bool {
	vimEnabledMux.Lock()
	defer vimEnabledMux.Unlock()
	return vimEnabled
}
