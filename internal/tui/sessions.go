package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/console"
)

// ctrl+r session picker: a fuzzy list over the console's resumable session
// snapshots (ListSessions is the same seam the /resume command and
// --resume read). Enter loads the snapshot through dispatch("/resume
// <name>") so the console owns the actual state restore.

// openSessions opens the session picker with an empty filter.
func (m *Model) openSessions() {
	m.overlay = overlaySessions
	m.sessionsFilter = ""
	m.sessionsSel = 0
	m.sessionsEntries = m.sessionsList()
}

// sessionsList resolves the snapshot list through the console (nil console
// leaves the picker empty).
func (m *Model) sessionsList() []console.SessionEntry {
	if m.console == nil {
		return nil
	}
	entries, err := m.console.ListSessions()
	if err != nil {
		return nil
	}
	return entries
}

// sessionsFiltered applies the fuzzy filter to the loaded entries.
func (m *Model) sessionsFiltered() []console.SessionEntry {
	filter := strings.ToLower(m.sessionsFilter)
	if filter == "" {
		return m.sessionsEntries
	}
	var out []console.SessionEntry
	for _, e := range m.sessionsEntries {
		if fuzzyMatch(strings.ToLower(e.Name), filter) {
			out = append(out, e)
		}
	}
	return out
}

// sessionsKey handles one keypress inside the session picker.
func (m *Model) sessionsKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.sessionsSel > 0 {
			m.sessionsSel--
		}
		return true, nil
	case "down":
		if m.sessionsSel < len(m.sessionsFiltered())-1 {
			m.sessionsSel++
		}
		return true, nil
	case "backspace":
		if n := len(m.sessionsFilter); n > 0 {
			m.sessionsFilter = m.sessionsFilter[:n-1]
			m.sessionsSel = 0
		}
		return true, nil
	case "enter":
		entries := m.sessionsFiltered()
		if m.sessionsSel >= len(entries) {
			return true, nil
		}
		choice := entries[m.sessionsSel]
		m.overlay = overlayNone
		if m.console == nil {
			return true, nil
		}
		// The console owns the state restore; the previous conversation
		// stays on disk under its own name (last-session tracks the newest
		// exit), so switching never loses it.
		m.remember("/resume " + choice.Name)
		return true, m.dispatch("/resume " + choice.Name)
	}
	if len(msg.Runes) > 0 {
		m.sessionsFilter += string(msg.Runes)
		m.sessionsSel = 0
		return true, nil
	}
	return false, nil
}

// sessionsView renders the picker: filter line, snapshot list with the
// selection highlighted, and the key legend.
func (m *Model) sessionsView() string {
	entries := m.sessionsFiltered()
	if m.sessionsSel >= len(entries) {
		m.sessionsSel = len(entries) - 1
	}
	if m.sessionsSel < 0 {
		m.sessionsSel = 0
	}
	var b strings.Builder
	b.WriteString(boldDimStyle.Render("resume session"))
	b.WriteString("\n")
	b.WriteString(promptStyle.Render("❯ ") + m.sessionsFilter + cursorStyle.Render(" "))
	b.WriteString("\n\n")
	max := m.height - 6
	if max < 1 {
		max = 1
	}
	start := 0
	if m.sessionsSel >= max {
		start = m.sessionsSel - max + 1
	}
	end := min(start+max, len(entries))
	for i := start; i < end; i++ {
		e := entries[i]
		row := padRight(e.Name, 24) + "  " + e.ModTime.Format("2006-01-02 15:04")
		if i == m.sessionsSel {
			b.WriteString(selStyle.Render("  "+row) + "\n")
		} else {
			b.WriteString(dimStyle.Render("  "+row) + "\n")
		}
	}
	if len(entries) == 0 {
		b.WriteString(dimStyle.Render("  (no saved sessions — name one with /rename <name>)") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("type to filter · ↑↓ select · enter resume · esc close"))
	return boxStyle.Render(b.String())
}
