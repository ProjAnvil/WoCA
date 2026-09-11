package tui

import (
	"fmt"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// Themes: named palettes that re-style the transcript. SetTheme reassigns
// the package styles; it runs on the bubbletea goroutine (the /theme
// command), so no locking is needed beyond the mutex guarding concurrent
// tests.
//
// Color discipline (mirrors the web monochrome decision, adapted for the
// terminal): chrome (prompt, borders, selection, tool chips) is themed and
// fully de-saturated by the mono theme; semantic colors (diff + red / −
// red, error red) are never disabled by a theme — they carry meaning, and
// NO_COLOR ( honored by lipgloss/termenv) is the only off switch.

type Theme struct {
	Name        string
	Description string
	// Mono drops every chrome color (bold/faint only); semantic diff and
	// error colors stay.
	Mono bool
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
	{Name: "mono", Description: "no chrome color (bold/faint); diff and error reds stay", Mono: true},
}

var (
	themeMu       sync.Mutex
	currentTheme  = "dark"
	vimEnabledMux sync.Mutex
	vimEnabled    bool
	// toolStyleMono de-saturates tool chips when the mono theme is active.
	toolStyleMono bool
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
	toolStyleMono = t.Mono
	dimStyle = lipgloss.NewStyle().Faint(true)
	boldDimStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	promptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.Prompt))
	userStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.User))
	boxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(t.Border))
	addStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	delStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	cursorStyle = lipgloss.NewStyle().Reverse(true)
	selStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(t.Sel)).Foreground(lipgloss.Color(t.SelText))
	selDescStyle = lipgloss.NewStyle().Background(lipgloss.Color(t.Sel)).Foreground(lipgloss.Color(t.SelText)).Faint(true)
	if t.Mono {
		promptStyle = lipgloss.NewStyle().Bold(true)
		userStyle = lipgloss.NewStyle().Bold(true)
		boxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
		selStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
		selDescStyle = lipgloss.NewStyle().Reverse(true).Faint(true)
	}
}

// toolCategory is the three-way tool classification behind the chips: hue
// only distinguishes execute / mutate / observe — the tool's name text is
// the identity channel, so three categories carry all the signal without
// five hues of noise.
type toolCategory int

const (
	toolObserve toolCategory = iota
	toolExecute
	toolMutate
)

// classifyTool maps a tool name to its category.
func classifyTool(name string) toolCategory {
	switch name {
	case "Bash":
		return toolExecute
	case "Edit", "Write", "NotebookEdit":
		return toolMutate
	default:
		return toolObserve
	}
}

// toolColor returns the chip style for one tool (category color; bold+faint
// under the mono theme).
func toolColor(name string) lipgloss.Style {
	if toolStyleMono {
		return lipgloss.NewStyle().Bold(true).Faint(true)
	}
	var color lipgloss.Color
	switch classifyTool(name) {
	case toolExecute:
		color = lipgloss.Color("2") // green
	case toolMutate:
		color = lipgloss.Color("3") // yellow
	default:
		color = lipgloss.Color("6") // cyan
	}
	return lipgloss.NewStyle().Bold(true).Foreground(color)
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
