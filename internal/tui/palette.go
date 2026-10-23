package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ctrl+k palette: a fuzzy command launcher over the shared slash-command
// table (the same registry the web palette and the "/" menu consume), plus a
// few TUI actions that are not slash commands. Execution always falls
// through dispatch/console.Execute — the palette never bypasses the command
// layer.

// paletteAction is a palette entry that is not a slash command.
type paletteAction struct{ name, desc string }

// paletteActions are the TUI-only entries (kept tiny on purpose; everything
// else is a console command).
var paletteActions = []paletteAction{
	{name: "review transcript", desc: "ctrl+o — fold/unfold the full transcript"},
	{name: "resume session", desc: "ctrl+r — pick a saved session snapshot"},
	{name: "switch model", desc: "pick from the model catalog"},
}

// paletteEntries renders the full candidate list: TUI actions first, then
// every console command.
func (m *Model) paletteEntries() []command {
	var out []command
	for _, a := range paletteActions {
		out = append(out, command{name: a.name, desc: a.desc})
	}
	// The slash command table already carries the group tags.
	out = append(out, slashCommands...)
	return out
}

// paletteFiltered returns the entries matching the filter (fuzzy
// subsequence, case-insensitive).
func (m *Model) paletteFiltered() []command {
	filter := strings.ToLower(m.paletteFilter)
	if filter == "" {
		return m.paletteEntries()
	}
	var out []command
	for _, c := range m.paletteEntries() {
		if fuzzyMatch(strings.ToLower(c.name), filter) {
			out = append(out, c)
		}
	}
	return out
}

// fuzzyMatch reports whether needle appears in hay as a subsequence.
func fuzzyMatch(hay, needle string) bool {
	if needle == "" {
		return true
	}
	i := 0
	for j := 0; j < len(hay) && i < len(needle); j++ {
		if hay[j] == needle[i] {
			i++
		}
	}
	return i == len(needle)
}

// openPalette opens the command palette with an empty filter.
func (m *Model) openPalette() {
	m.overlay = overlayPalette
	m.paletteFilter = ""
	m.paletteSel = 0
}

// paletteKey handles one keypress inside the palette overlay.
func (m *Model) paletteKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.paletteSel > 0 {
			m.paletteSel--
		}
		return true, nil
	case "down":
		if m.paletteSel < len(m.paletteFiltered())-1 {
			m.paletteSel++
		}
		return true, nil
	case "backspace":
		if n := len(m.paletteFilter); n > 0 {
			m.paletteFilter = m.paletteFilter[:n-1]
			m.paletteSel = 0
		}
		return true, nil
	case "enter":
		entries := m.paletteFiltered()
		if m.paletteSel >= len(entries) {
			return true, nil
		}
		choice := entries[m.paletteSel]
		m.overlay = overlayNone
		switch choice.name {
		case "review transcript":
			m.openReview()
			return true, nil
		case "resume session":
			m.openSessions()
			return true, nil
		case "switch model":
			m.openModels()
			return true, nil
		default:
			m.remember(choice.name)
			return true, m.dispatch(choice.name)
		}
	}
	if len(msg.Runes) > 0 {
		m.paletteFilter += string(msg.Runes)
		m.paletteSel = 0
		return true, nil
	}
	return false, nil
}

// paletteView renders the palette: filter line, fuzzy-filtered entries with
// the selection highlighted, and the key legend.
func (m *Model) paletteView() string {
	entries := m.paletteFiltered()
	if m.paletteSel >= len(entries) {
		m.paletteSel = len(entries) - 1
	}
	if m.paletteSel < 0 {
		m.paletteSel = 0
	}
	var b strings.Builder
	b.WriteString(boldDimStyle.Render("command palette"))
	b.WriteString("\n")
	b.WriteString(promptStyle.Render("❯ ") + m.paletteFilter + cursorStyle.Render(" "))
	b.WriteString("\n\n")
	const col = 22
	max := m.height - 6
	if max < 1 {
		max = 1
	}
	start := 0
	if m.paletteSel >= max {
		start = m.paletteSel - max + 1
	}
	end := min(start+max, len(entries))
	for i := start; i < end; i++ {
		c := entries[i]
		if i == m.paletteSel {
			b.WriteString(selStyle.Render("  "+padRight(c.name, col)) + selDescStyle.Render("  "+c.desc) + "\n")
		} else {
			b.WriteString(dimStyle.Render("  "+padRight(c.name, col)+"  "+c.desc) + "\n")
		}
	}
	if len(entries) == 0 {
		b.WriteString(dimStyle.Render("  (no matching commands)") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("type to filter · ↑↓ select · enter run · esc close"))
	return boxStyle.Render(b.String())
}
