package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/tools"
)

// The decision surface: every run interrupt (permission approval, plan
// approval, AskUserQuestion batch) resolves through one interaction model —
// the shortcut keys stay fixed (y = allow, n/esc = deny, free text for
// questions), enter executes the highlighted option, and the highlight
// defaults to the safe choice (deny). AskUserQuestion batches render their
// structured options as a selectable list, one question at a time, instead
// of making the user hand-type option labels.

// decisionOptions is the fixed allow/deny pair for permission and plan
// approvals; index 0 is allow, 1 deny (the default highlight).
var decisionOptions = [2]string{"allow", "deny"}

// pendingBegin initializes the selection state for a freshly arrived
// interrupt.
func (m *Model) pendingBegin() {
	switch v := m.pending.value.(type) {
	case agent.PermissionRequest, agent.SubagentInterrupt, agent.PlanApprovalRequest:
		m.decSel = 1 // safe default: deny
	case agent.AskUserRequest:
		m.askIdx = 0
		m.askAnswers = nil
		if len(v.Questions) == 0 {
			m.resolvePending(agent.AskUserAnswer{})
			return
		}
		m.askResetFor(v.Questions[0])
	}
}

// askResetFor resets the per-question selection state.
func (m *Model) askResetFor(q tools.Question) {
	m.askSel = 0
	m.askPicks = make([]bool, len(q.Options))
}

// resolvePendingKey handles a keypress while an interrupt awaits a decision.
func (m *Model) resolvePendingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.pending.value.(type) {
	case agent.PermissionRequest, agent.SubagentInterrupt, agent.PlanApprovalRequest:
		switch msg.String() {
		case "y", "Y":
			m.resolvePending("y")
		case "n", "N", "esc":
			m.resolvePending("n")
		case "left", "shift+tab":
			m.decSel = (m.decSel + len(decisionOptions) - 1) % len(decisionOptions)
		case "right", "tab":
			m.decSel = (m.decSel + 1) % len(decisionOptions)
		case "enter":
			if m.decSel == 0 {
				m.resolvePending("y")
			} else {
				m.resolvePending("n")
			}
		}
		return m, nil
	case agent.AskUserRequest:
		return m.askKey(msg)
	}
	return m, nil
}

// askKey handles one keypress for an AskUserQuestion batch: questions with
// options render as a selectable list (space toggles in multi-select mode);
// questions without options collect free text on the input line.
func (m *Model) askKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	req, ok := m.pending.value.(agent.AskUserRequest)
	if !ok || m.askIdx >= len(req.Questions) {
		m.resolvePending(agent.AskUserAnswer{})
		return m, nil
	}
	q := req.Questions[m.askIdx]
	// esc aborts the whole batch: remaining questions get empty answers.
	abort := func() {
		for i := m.askIdx; i < len(req.Questions); i++ {
			m.askAnswers = append(m.askAnswers, "")
		}
		m.resolvePending(agent.AskUserAnswer{Answers: m.askAnswers})
	}
	if len(q.Options) > 0 {
		switch msg.String() {
		case "up":
			if m.askSel > 0 {
				m.askSel--
			}
		case "down":
			if m.askSel < len(q.Options)-1 {
				m.askSel++
			}
		case "space", " ":
			if q.MultiSelect && m.askSel < len(m.askPicks) {
				m.askPicks[m.askSel] = !m.askPicks[m.askSel]
			}
		case "enter":
			if q.MultiSelect {
				var picks []string
				for i, on := range m.askPicks {
					if on {
						picks = append(picks, q.Options[i].Label)
					}
				}
				if len(picks) == 0 {
					picks = []string{q.Options[m.askSel].Label}
				}
				m.askAnswers = append(m.askAnswers, strings.Join(picks, ", "))
			} else {
				m.askAnswers = append(m.askAnswers, q.Options[m.askSel].Label)
			}
			m.askAdvance()
		case "esc":
			abort()
		case "ctrl+c", "ctrl+d":
			m.resolvePending(agent.AskUserAnswer{})
			return m, tea.Quit
		}
		return m, nil
	}
	// Free-text question: the input line collects the answer.
	switch msg.String() {
	case "enter":
		answer := strings.TrimSpace(m.input)
		m.input = ""
		m.cursor = 0
		m.askAnswers = append(m.askAnswers, answer)
		m.askAdvance()
	case "backspace":
		if m.cursor > 0 {
			m.input = m.input[:m.cursor-1] + m.input[m.cursor:]
			m.cursor--
		}
	case "esc":
		abort()
	case "ctrl+c", "ctrl+d":
		m.resolvePending(agent.AskUserAnswer{})
		return m, tea.Quit
	default:
		if len(msg.Runes) > 0 {
			r := string(msg.Runes)
			m.input = m.input[:m.cursor] + r + m.input[m.cursor:]
			m.cursor += len(msg.Runes)
		}
	}
	return m, nil
}

// askAdvance moves to the next question or resolves the batch.
func (m *Model) askAdvance() {
	req, ok := m.pending.value.(agent.AskUserRequest)
	m.askIdx++
	if ok && m.askIdx < len(req.Questions) {
		m.askResetFor(req.Questions[m.askIdx])
		return
	}
	m.resolvePending(agent.AskUserAnswer{Answers: m.askAnswers})
}

// pendingPrompt renders the body of the pending interrupt: the question with
// its selectable options, or the plain approval line with the allow/deny
// pair.
func (m *Model) pendingPrompt() string {
	switch v := m.pending.value.(type) {
	case agent.PermissionRequest:
		return fmt.Sprintf("Allow %s? — %s\n%s", v.ToolName, v.Reason, m.decisionRow())
	case agent.SubagentInterrupt:
		return fmt.Sprintf("Subagent %q requests permission: %s — %s\n%s", v.Subagent, v.Inner.ToolName, v.Inner.Reason, m.decisionRow())
	case agent.PlanApprovalRequest:
		return fmt.Sprintf("Approve this plan?\n%s", m.decisionRow())
	case agent.AskUserRequest:
		if m.askIdx >= len(v.Questions) {
			return "Continue?"
		}
		q := v.Questions[m.askIdx]
		var b strings.Builder
		title := q.Question
		if q.Header != "" {
			title = q.Header + ": " + q.Question
		}
		if len(v.Questions) > 1 {
			fmt.Fprintf(&b, "(%d/%d) %s\n", m.askIdx+1, len(v.Questions), title)
		} else {
			fmt.Fprintf(&b, "%s\n", title)
		}
		for i, opt := range q.Options {
			mark := " "
			if q.MultiSelect && i < len(m.askPicks) && m.askPicks[i] {
				mark = "x"
			}
			row := fmt.Sprintf("[%s] %s", mark, opt.Label)
			if opt.Description != "" {
				row += dimStyle.Render(" — " + opt.Description)
			}
			if i == m.askSel {
				b.WriteString("  " + selStyle.Render(row) + "\n")
			} else {
				b.WriteString("  " + row + "\n")
			}
		}
		if len(q.Options) == 0 {
			b.WriteString("  " + dimStyle.Render("(type your answer, enter to submit)") + "\n")
		}
		return b.String()
	}
	return "Continue?"
}

// decisionRow renders the allow/deny pair with the highlighted (safe-default
// deny) option.
func (m *Model) decisionRow() string {
	cells := make([]string, len(decisionOptions))
	for i, opt := range decisionOptions {
		if i == m.decSel {
			cells[i] = selStyle.Render(" " + opt + " ")
		} else {
			cells[i] = dimStyle.Render(" " + opt + " ")
		}
	}
	return "  " + strings.Join(cells, "  ")
}
