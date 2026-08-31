package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/console"
)

// The /model catalog picker: a fuzzy list over the console's shared model
// catalog (Console.Models — the same wocadb.ModelPreset rows the web UI's
// /api/models serves). Selecting an entry dispatches "/model <id>", so the
// switch always goes through the existing ModelFactory path; the picker only
// replaces free-typing the id.

// openModels opens the catalog picker with an empty filter.
func (m *Model) openModels() {
	m.overlay = overlayModels
	m.modelsFilter = ""
	m.modelsSel = 0
	m.modelsEntries = m.modelsList()
}

// modelsList resolves the catalog through the console (nil console or an
// empty catalog leaves the picker empty).
func (m *Model) modelsList() []console.ModelEntry {
	if m.console == nil {
		return nil
	}
	return m.console.Models()
}

// hasModelCatalog reports whether a bare /model should open the picker.
func (m *Model) hasModelCatalog() bool {
	return len(m.modelsList()) > 0
}

// modelsFiltered applies the fuzzy filter to the catalog.
func (m *Model) modelsFiltered() []console.ModelEntry {
	filter := strings.ToLower(m.modelsFilter)
	if filter == "" {
		return m.modelsEntries
	}
	var out []console.ModelEntry
	for _, e := range m.modelsEntries {
		hay := strings.ToLower(e.Name + " " + e.Provider + " " + e.Model)
		if fuzzyMatch(hay, filter) {
			out = append(out, e)
		}
	}
	return out
}

// modelsKey handles one keypress inside the catalog picker.
func (m *Model) modelsKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.modelsSel > 0 {
			m.modelsSel--
		}
		return true, nil
	case "down":
		if m.modelsSel < len(m.modelsFiltered())-1 {
			m.modelsSel++
		}
		return true, nil
	case "backspace":
		if n := len(m.modelsFilter); n > 0 {
			m.modelsFilter = m.modelsFilter[:n-1]
			m.modelsSel = 0
		}
		return true, nil
	case "enter":
		entries := m.modelsFiltered()
		if m.modelsSel >= len(entries) {
			return true, nil
		}
		choice := entries[m.modelsSel]
		m.overlay = overlayNone
		m.remember("/model " + choice.Model)
		return true, m.dispatch("/model " + choice.Model)
	}
	if len(msg.Runes) > 0 {
		m.modelsFilter += string(msg.Runes)
		m.modelsSel = 0
		return true, nil
	}
	return false, nil
}

// modelsView renders the picker: filter line, catalog rows (current model
// marked), and the key legend.
func (m *Model) modelsView() string {
	entries := m.modelsFiltered()
	if m.modelsSel >= len(entries) {
		m.modelsSel = len(entries) - 1
	}
	if m.modelsSel < 0 {
		m.modelsSel = 0
	}
	model := m.cfg.Model
	if m.console != nil {
		model = m.console.Cfg().Model
	}
	var b strings.Builder
	b.WriteString(boldDimStyle.Render("switch model"))
	b.WriteString("\n")
	b.WriteString(promptStyle.Render("❯ ") + m.modelsFilter + cursorStyle.Render(" "))
	b.WriteString("\n\n")
	max := m.height - 6
	if max < 1 {
		max = 1
	}
	start := 0
	if m.modelsSel >= max {
		start = m.modelsSel - max + 1
	}
	end := start + max
	if end > len(entries) {
		end = len(entries)
	}
	for i := start; i < end; i++ {
		e := entries[i]
		marker := "  "
		if e.Model == model {
			marker = "* "
		}
		row := marker + padRight(e.Name, 22) + padRight(e.Provider, 11) + e.Model
		if i == m.modelsSel {
			b.WriteString(selStyle.Render("  "+row) + "\n")
		} else {
			b.WriteString(dimStyle.Render("  "+row) + "\n")
		}
	}
	if len(entries) == 0 {
		b.WriteString(dimStyle.Render("  (no matching models)") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("type to filter · ↑↓ select · enter switch (ModelFactory) · esc close"))
	return boxStyle.Render(b.String())
}
