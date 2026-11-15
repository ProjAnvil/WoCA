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
	case agent.ApprovalRequest:
		m.apprIdx = 0
		m.apprDec = map[string]agent.AskDecision{}
		if len(v.Asks) == 0 {
			m.resolvePending(agent.ApprovalAnswer{Decisions: m.apprDec})
			return
		}
		m.apprEnter(v)
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

// apprEnter sets up the per-ask interaction state for the envelope's
// current ask (permission/plan asks get the allow/deny pair, AskUserQuestion
// asks reuse the sequential question machine).
func (m *Model) apprEnter(req agent.ApprovalRequest) {
	ask := req.Asks[m.apprIdx]
	if ask.ToolName == "AskUserQuestion" {
		if len(ask.Questions) == 0 {
			// Nothing to ask: auto-allow and keep walking the envelope.
			m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeAllow})
			return
		}
		m.askIdx = 0
		m.askAnswers = nil
		m.askResetFor(ask.Questions[0])
		return
	}
	m.decSel = 1 // safe default: deny
}

// apprAdvance records the current ask's decision and moves to the next ask,
// resolving the whole envelope with one ApprovalAnswer after the last.
func (m *Model) apprAdvance(dec agent.AskDecision) {
	req, ok := m.pending.value.(agent.ApprovalRequest)
	if !ok || m.apprIdx >= len(req.Asks) {
		return
	}
	m.apprDec[req.Asks[m.apprIdx].CallID] = dec
	m.apprIdx++
	if m.apprIdx < len(req.Asks) {
		m.apprEnter(req)
		return
	}
	m.resolvePending(agent.ApprovalAnswer{Decisions: m.apprDec})
}

// apprAbort resolves the envelope denying every ask that has not been
// answered yet (ctrl+c / ctrl+d on any ask).
func (m *Model) apprAbort() {
	req, ok := m.pending.value.(agent.ApprovalRequest)
	if ok {
		for ; m.apprIdx < len(req.Asks); m.apprIdx++ {
			m.apprDec[req.Asks[m.apprIdx].CallID] = agent.AskDecision{Outcome: agent.OutcomeDeny}
		}
	}
	m.resolvePending(agent.ApprovalAnswer{Decisions: m.apprDec})
}

// currentQuestions returns the question list driving the ask machine: a bare
// AskUserRequest's, or the current ApprovalRequest ask's questions.
func (m *Model) currentQuestions() ([]tools.Question, bool) {
	switch v := m.pending.value.(type) {
	case agent.AskUserRequest:
		return v.Questions, true
	case agent.ApprovalRequest:
		if m.apprIdx < len(v.Asks) && v.Asks[m.apprIdx].ToolName == "AskUserQuestion" {
			return v.Asks[m.apprIdx].Questions, true
		}
	}
	return nil, false
}

// askAbort resolves the current ask batch the way the interrupt shape
// demands (an envelope continues with its remaining asks denied; a bare
// AskUserRequest resolves with empty answers).
func (m *Model) askAbort() {
	if _, isApproval := m.pending.value.(agent.ApprovalRequest); isApproval {
		m.apprAbort()
		return
	}
	m.resolvePending(agent.AskUserAnswer{})
}

// askResetFor resets the per-question selection state.
func (m *Model) askResetFor(q tools.Question) {
	m.askSel = 0
	m.askPicks = make([]bool, len(q.Options))
}

// resolvePendingKey handles a keypress while an interrupt awaits a decision.
func (m *Model) resolvePendingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch v := m.pending.value.(type) {
	case agent.ApprovalRequest:
		if m.apprIdx >= len(v.Asks) {
			return m, nil
		}
		ask := v.Asks[m.apprIdx]
		if ask.ToolName == "AskUserQuestion" {
			return m.askKey(msg)
		}
		switch msg.String() {
		case "y", "Y":
			m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeAllow})
		case "n", "N", "esc":
			m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeDeny})
		case "left", "shift+tab":
			m.decSel = (m.decSel + len(decisionOptions) - 1) % len(decisionOptions)
		case "right", "tab":
			m.decSel = (m.decSel + 1) % len(decisionOptions)
		case "enter":
			if m.decSel == 0 {
				m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeAllow})
			} else {
				m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeDeny})
			}
		}
		return m, nil
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
// questions without options collect free text on the input line. Inside a
// Task-5 ApprovalRequest envelope the machine walks the CURRENT ask's
// questions (see currentQuestions) and hands the collected answers back to
// the envelope walker.
func (m *Model) askKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	questions, ok := m.currentQuestions()
	if !ok || m.askIdx >= len(questions) {
		m.askAbort()
		return m, nil
	}
	q := questions[m.askIdx]
	// esc aborts the whole batch: remaining questions get empty answers.
	abort := func() {
		for i := m.askIdx; i < len(questions); i++ {
			m.askAnswers = append(m.askAnswers, "")
		}
		m.askAdvance()
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
			return m.askAbortQuit()
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
		return m.askAbortQuit()
	default:
		if len(msg.Runes) > 0 {
			r := string(msg.Runes)
			m.input = m.input[:m.cursor] + r + m.input[m.cursor:]
			m.cursor += len(msg.Runes)
		}
	}
	return m, nil
}

// askAbortQuit is the ctrl+c/ctrl+d path: the batch aborts (empty answers,
// or the envelope's remaining asks denied) and the program quits.
func (m *Model) askAbortQuit() (tea.Model, tea.Cmd) {
	m.askAbort()
	return m, tea.Quit
}

// askAdvance moves to the next question or resolves the batch — inside an
// envelope, finishing one ask's questions records that ask's answers and
// walks to the next ask.
func (m *Model) askAdvance() {
	questions, ok := m.currentQuestions()
	m.askIdx++
	if ok && m.askIdx < len(questions) {
		m.askResetFor(questions[m.askIdx])
		return
	}
	if req, isApproval := m.pending.value.(agent.ApprovalRequest); isApproval && m.apprIdx < len(req.Asks) {
		m.apprAdvance(agent.AskDecision{Outcome: agent.OutcomeAllow, Answers: m.askAnswers})
		return
	}
	m.resolvePending(agent.AskUserAnswer{Answers: m.askAnswers})
}

// pendingPrompt renders the body of the pending interrupt: the question with
// its selectable options, or the plain approval line with the allow/deny
// pair. A Task-5 ApprovalRequest envelope renders one ask at a time, with a
// position marker when several are queued.
func (m *Model) pendingPrompt() string {
	switch v := m.pending.value.(type) {
	case agent.ApprovalRequest:
		if m.apprIdx >= len(v.Asks) {
			return "Continue?"
		}
		ask := v.Asks[m.apprIdx]
		marker := ""
		if len(v.Asks) > 1 {
			marker = fmt.Sprintf("ask %d/%d — ", m.apprIdx+1, len(v.Asks))
		}
		switch ask.ToolName {
		case "AskUserQuestion":
			return marker + m.askUserPrompt(ask.Questions)
		case "ExitPlanMode":
			plan := ask.Plan
			if plan != "" {
				plan = "\n" + dimStyle.Render(truncate(plan, 400)) + "\n"
			}
			return marker + "Approve this plan?" + plan + "\n" + m.decisionRow()
		default:
			return fmt.Sprintf("%sAllow %s? — %s\n%s", marker, ask.ToolName, ask.Reason, m.decisionRow())
		}
	case agent.PermissionRequest:
		return fmt.Sprintf("Allow %s? — %s\n%s", v.ToolName, v.Reason, m.decisionRow())
	case agent.SubagentInterrupt:
		return fmt.Sprintf("Subagent %q requests permission: %s — %s\n%s", v.Subagent, v.Inner.ToolName, v.Inner.Reason, m.decisionRow())
	case agent.PlanApprovalRequest:
		return fmt.Sprintf("Approve this plan?\n%s", m.decisionRow())
	case agent.AskUserRequest:
		return m.askUserPrompt(v.Questions)
	}
	return "Continue?"
}

// askUserPrompt renders the sequential question UI for one question batch
// (shared by the bare AskUserRequest and envelope ask_user asks).
func (m *Model) askUserPrompt(questions []tools.Question) string {
	if m.askIdx >= len(questions) {
		return "Continue?"
	}
	q := questions[m.askIdx]
	var b strings.Builder
	title := q.Question
	if q.Header != "" {
		title = q.Header + ": " + q.Question
	}
	if len(questions) > 1 {
		fmt.Fprintf(&b, "(%d/%d) %s\n", m.askIdx+1, len(questions), title)
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
