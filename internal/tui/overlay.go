package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// The overlay layer: one full-screen surface above the base view. The base
// (transcript + composer + status) keeps rendering underneath and runs keep
// streaming into it while an overlay is open; esc always pops back to base.
// Overlays never nest in this build — ctrl+k/ctrl+r/ctrl+o are base-level
// keys, so at most one is open at a time.

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayReview
	overlayPalette
	overlaySessions
	overlayModels
)

func (k overlayKind) String() string {
	switch k {
	case overlayReview:
		return "review"
	case overlayPalette:
		return "palette"
	case overlaySessions:
		return "sessions"
	case overlayModels:
		return "models"
	}
	return "none"
}

// overlayKey routes one keypress to the open overlay. It reports whether the
// key was consumed (unconsumed keys fall through to the base handler) and
// forwards any command an overlay action produced.
func (m *Model) overlayKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if m.overlay == overlayNone {
		return false, nil
	}
	switch msg.String() {
	case "esc", "q":
		m.overlay = overlayNone
		return true, nil
	case "ctrl+c", "ctrl+d":
		// Closing the overlay is the safe reading of an exit key while
		// browsing — accidental exits must not kill the session.
		m.overlay = overlayNone
		return true, nil
	}
	switch m.overlay {
	case overlayReview:
		return m.reviewKey(msg), nil
	case overlayPalette:
		return m.paletteKey(msg)
	case overlaySessions:
		return m.sessionsKey(msg)
	case overlayModels:
		return m.modelsKey(msg)
	}
	return false, nil
}

// openReview opens the transcript review overlay anchored at the newest
// block.
func (m *Model) openReview() {
	m.overlay = overlayReview
	if m.reviewSel >= len(m.blocks) || m.reviewSel < 0 {
		m.reviewSel = len(m.blocks) - 1
	}
	if m.reviewSel < 0 {
		m.reviewSel = 0
	}
}

// reviewKey handles one keypress inside the review overlay.
func (m *Model) reviewKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "up":
		if m.reviewSel > 0 {
			m.reviewSel--
		}
		return true
	case "down":
		if m.reviewSel < len(m.blocks)-1 {
			m.reviewSel++
		}
		return true
	case "pgup":
		m.reviewSel -= 10
		if m.reviewSel < 0 {
			m.reviewSel = 0
		}
		return true
	case "pgdown":
		m.reviewSel += 10
		if m.reviewSel > len(m.blocks)-1 {
			m.reviewSel = len(m.blocks) - 1
		}
		return true
	case "enter", " ":
		if m.reviewSel < len(m.blocks) {
			m.reviewFolded[m.reviewSel] = !m.reviewFold(m.reviewSel)
		}
		return true
	case "{":
		m.reviewSel = m.prevUserBlock(m.reviewSel)
		return true
	case "}":
		m.reviewSel = m.nextUserBlock(m.reviewSel)
		return true
	}
	return false
}

// prevUserBlock finds the user block strictly before i (or i itself when
// none exists).
func (m *Model) prevUserBlock(i int) int {
	for j := i - 1; j >= 0; j-- {
		if m.blocks[j].kind == blockUser {
			return j
		}
	}
	return i
}

// nextUserBlock finds the user block strictly after i (or i itself).
func (m *Model) nextUserBlock(i int) int {
	for j := i + 1; j < len(m.blocks); j++ {
		if m.blocks[j].kind == blockUser {
			return j
		}
	}
	return i
}

// overlayView renders the open overlay full-screen.
func (m *Model) overlayView() string {
	switch m.overlay {
	case overlayReview:
		return m.reviewView()
	case overlayPalette:
		return m.paletteView()
	case overlaySessions:
		return m.sessionsView()
	case overlayModels:
		return m.modelsView()
	}
	return ""
}
