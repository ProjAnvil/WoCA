package tui

import (
	"strings"
	"time"
)

// Batch-0 interaction plumbing: run interruption (double-esc / ctrl+c), the
// parked next-message slot, and decision-box draft preservation.

// escDoubleWindow is the maximum gap between two esc presses that counts as
// an interrupt (the web UI uses the same threshold).
const escDoubleWindow = 300 * time.Millisecond

// ctrlCExitWindow is the window in which a second idle ctrl+c exits.
const ctrlCExitWindow = 800 * time.Millisecond

// queuedMsg is one parked prompt typed while a run is in flight.
type queuedMsg struct {
	text string
}

// interrupt aborts the in-flight run (double-esc or ctrl+c while running).
// The canceled run still delivers its runDone, which renders the interrupt
// marker and flushes any parked message.
func (m *Model) interrupt() {
	if !m.running || m.cancelRun == nil {
		return
	}
	m.interrupting = true
	m.write(dimStyle.Render("⏹ interrupting…") + "\n")
	m.cancelRun()
}

// park stores the current input line as the next message, delivered when the
// run goes idle. Single slot, last-write-wins (web parity); ↑ on an empty
// line takes it back for editing.
func (m *Model) park() {
	text := strings.TrimSpace(m.input)
	if text == "" {
		return
	}
	m.queued = &queuedMsg{text: text}
	m.input = ""
	m.cursor = 0
	m.sel = 0
}

// holdDraft parks the composer line while a decision box owns the keyboard,
// so approval keys can never swallow typing in progress.
func (m *Model) holdDraft() {
	if m.input == "" {
		return
	}
	m.draftHeld = true
	m.draft = m.input
	m.input = ""
	m.cursor = 0
}

// restoreDraft puts a held composer line back after the decision resolved.
func (m *Model) restoreDraft() {
	if !m.draftHeld {
		return
	}
	m.draftHeld = false
	m.input = m.draft
	m.cursor = len(m.input)
	m.draft = ""
}
