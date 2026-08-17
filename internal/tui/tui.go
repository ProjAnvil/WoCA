// Package tui provides a bubbletea terminal interface for the woca agent: a
// scrolling transcript that updates live as the model streams, and an input
// line.
package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/projanvil/langchain-golang/core/messages"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/notify"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// lipgloss styles for the terminal look.
var (
	dimStyle     = lipgloss.NewStyle().Faint(true)
	boldDimStyle = lipgloss.NewStyle().Bold(true).Faint(true)
	promptStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")) // ❯ cyan
	userStyle    = lipgloss.NewStyle().Bold(true)
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	addStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green +line
	delStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red -line
	cursorStyle  = lipgloss.NewStyle().Reverse(true)
	selStyle     = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("5")).Foreground(lipgloss.Color("15"))
	selDescStyle = lipgloss.NewStyle().Background(lipgloss.Color("5")).Foreground(lipgloss.Color("15")).Faint(true)
)

// toolColor returns a per-tool accent color used for colored tool-name chips.
func toolColor(name string) lipgloss.Style {
	color := lipgloss.Color("6") // cyan default
	switch name {
	case "Bash":
		color = lipgloss.Color("2") // green
	case "Read", "Glob", "Grep":
		color = lipgloss.Color("6") // cyan
	case "Edit", "Write", "NotebookEdit":
		color = lipgloss.Color("3") // yellow
	case "TodoWrite", "Task", "TaskCreate", "TaskUpdate":
		color = lipgloss.Color("5") // magenta
	case "WebFetch", "WebSearch":
		color = lipgloss.Color("4") // blue
	}
	return lipgloss.NewStyle().Bold(true).Foreground(color)
}

// streamMsg carries one increment of live output.
type streamMsg string

// todosMsg carries a fresh todo-list snapshot for live updates.
type todosMsg []tools.Todo

// inFlightMsg carries the names of currently running tools (for the spinner
// tree).
type inFlightMsg []string

// interruptMsg carries a run interrupt to the UI; the run goroutine blocks
// until the answer arrives on answerCh (then the runner resumes via
// StreamResume).
type interruptMsg struct {
	value    any
	answerCh chan any
}

// bannerLines is the welcome banner shown when the transcript is empty.
func (m *Model) bannerLines() []string {
	logo := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")).
		Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("208")).
		Render("  ✻ woca · coding agent  ")
	welcome := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")).Render("✻ Welcome to woca") +
		" " + dimStyle.Render("v"+m.version)
	return []string{
		logo,
		welcome,
		dimStyle.Render("  /help for help · /settings to configure · /permissions <mode> to change permissions"),
		dimStyle.Render("  ! for bash mode · esc to interrupt · ctrl-c to exit"),
	}
}

type runDone struct {
	result []messages.Message
	err    error
}

// Model is the bubbletea application state.
type Model struct {
	agent     *agent.Agent
	cfg       config.Config
	console   *console.Console
	version   string
	history   []messages.Message
	todos     []tools.Todo
	input     string
	lines     []string
	cur       string
	running   bool
	ch        <-chan tea.Msg
	viewport  *Viewport
	height    int
	inFlight  []string
	spinner   spinner.Model
	cursor    int
	inputHist []string
	histIdx   int
	sel       int
	// pending is the interrupt awaiting a user decision (permission
	// approval, plan approval, or an AskUserQuestion batch).
	pending *interruptMsg
	// vimInsert is the insert/normal sub-state of vim input mode (only
	// meaningful when VimMode() is on).
	vimInsert bool
	// vimPendingD tracks a pending "dd" (delete line) in normal mode.
	vimPendingD bool
	// lastPrompt feeds the observability run_start event.
	lastPrompt string
	// settings backs the per-run debug-log dynamic refresh (optional).
	settings *settings.Store
}

// New creates the TUI model.
func New(a *agent.Agent, history []messages.Message, cfg config.Config) *Model {
	return &Model{
		agent:    a,
		history:  history,
		cfg:      cfg,
		version:  "0.3.0",
		viewport: NewViewport(),
		height:   24,
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
}

// SetConsole attaches the shared console command layer (/web, /clear, /model,
// ...). Without it only /help works.
func (m *Model) SetConsole(c *console.Console) { m.console = c }

// Init returns the initial command (starts the spinner ticker).
func (m *Model) Init() tea.Cmd { return m.spinner.Tick }

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height - 2
		if m.height < 1 {
			m.height = 1
		}
		m.viewport.SetHeight(m.height)
	case tea.KeyMsg:
		// A pending interrupt resolves first: y/enter approves, n/esc denies
		// (permission and plan approval); AskUserQuestion batches type their
		// answer into the input line and submit with enter.
		if m.pending != nil {
			return m.resolvePendingKey(msg)
		}
		// Vim modal input (/vim): in normal mode keys edit,
		// enter still submits. Interception happens before the standard
		// handling; enter falls through to it.
		if VimMode() && !m.vimInsert && !m.running {
			if handled := m.vimKey(msg); handled {
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "esc":
			if VimMode() && m.vimInsert {
				m.vimInsert = false
			}
		case "ctrl+a":
			if !m.running {
				m.cursor = 0
			}
		case "ctrl+e":
			if !m.running {
				m.cursor = len(m.input)
			}
		case "pgup":
			m.viewport.PageUp()
		case "pgdown":
			m.viewport.PageDown()
		case "up":
			if m.running {
				break
			}
			if cmds := suggestions(m.input); len(cmds) > 0 {
				if m.sel >= len(cmds) {
					m.sel = len(cmds) - 1
				}
				if m.sel > 0 {
					m.sel--
				}
			} else if m.histIdx > 0 {
				m.histIdx--
				m.input = m.inputHist[m.histIdx]
				m.cursor = len(m.input)
			}
		case "down":
			if m.running {
				break
			}
			if cmds := suggestions(m.input); len(cmds) > 0 {
				if m.sel >= len(cmds) {
					m.sel = len(cmds) - 1
				}
				if m.sel < len(cmds)-1 {
					m.sel++
				}
			} else if m.histIdx < len(m.inputHist) {
				m.histIdx++
				if m.histIdx == len(m.inputHist) {
					m.input = ""
				} else {
					m.input = m.inputHist[m.histIdx]
				}
				m.cursor = len(m.input)
			}
		case "tab":
			if !m.running && strings.HasPrefix(m.input, "/") {
				if cmds := suggestions(m.input); len(cmds) > 0 {
					if m.sel >= len(cmds) {
						m.sel = len(cmds) - 1
					}
					m.input = cmds[m.sel].name
					m.cursor = len(m.input)
					m.sel = 0
				}
			}
		case "ctrl+u":
			if !m.running {
				m.input = ""
				m.cursor = 0
				m.sel = 0
			}
		case "left":
			if !m.running && m.cursor > 0 {
				m.cursor--
			}
		case "right":
			if !m.running && m.cursor < len(m.input) {
				m.cursor++
			}
		case "enter":
			if m.running {
				return m, nil
			}
			// In the command menu, Enter first completes the highlighted
			// command; a second Enter runs it.
			if strings.HasPrefix(m.input, "/") {
				if cmds := suggestions(m.input); len(cmds) > 0 {
					sel := m.sel
					if sel >= len(cmds) {
						sel = len(cmds) - 1
					}
					if strings.TrimSpace(m.input) != cmds[sel].name {
						m.input = cmds[sel].name
						m.cursor = len(m.input)
						m.sel = 0
						return m, nil
					}
				}
			}
			text := strings.TrimSpace(m.input)
			m.input = ""
			m.cursor = 0
			if text == "" {
				return m, nil
			}
			if len(m.inputHist) == 0 || m.inputHist[len(m.inputHist)-1] != text {
				m.inputHist = append(m.inputHist, text)
			}
			m.histIdx = len(m.inputHist)
			if text == "/exit" || text == "/quit" || text == "/q" {
				return m, tea.Quit
			}
			if handled, cmd := m.handleCommand(text); handled {
				return m, cmd
			}
			return m, m.submit(text)
		case "backspace":
			if !m.running && m.cursor > 0 {
				m.input = m.input[:m.cursor-1] + m.input[m.cursor:]
				m.cursor--
			}
		case "delete":
			if !m.running && m.cursor < len(m.input) {
				m.input = m.input[:m.cursor] + m.input[m.cursor+1:]
			}
		default:
			if !m.running && len(msg.Runes) > 0 {
				r := string(msg.Runes)
				m.input = m.input[:m.cursor] + r + m.input[m.cursor:]
				m.cursor += len(msg.Runes)
				m.sel = 0
			}
		}
	case streamMsg:
		m.write(string(msg))
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case todosMsg:
		m.todos = msg
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case inFlightMsg:
		m.inFlight = msg
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case interruptMsg:
		// A run is paused awaiting this decision; render the approval UI and
		// keep draining the run channel while the user decides.
		m.pending = &msg
		notify.Attention(os.Stdout, "woca: awaiting your decision")
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case runDone:
		m.running = false
		m.flush()
		m.ch = nil
		if msg.err != nil {
			m.write("Error: " + msg.err.Error() + "\n")
			notify.Attention(os.Stdout, "woca: run failed")
		} else {
			m.history = stripSystem(msg.result)
			notify.Attention(os.Stdout, "woca: response ready")
		}
		if m.agent != nil {
			m.todos = m.agent.Todos()
		}
		// Goal continuation scheduling (Codex's idle detection): after a run
		// finishes and the session is idle, an active goal keeps looping.
		if msg.err == nil {
			return m, m.maybeKickGoal()
		}
	}
	return m, nil
}

// obsSeq numbers TUI runs for the observability log.
var obsSeq int32

// obsBegin scopes observability events to this run and logs its start. The
// debug-log setting is re-read first so toggles made in other processes (the
// web UI on another serve instance) apply here dynamically.
func (m *Model) obsBegin(prompt string) string {
	if m.agent == nil || m.agent.Obs() == nil {
		return ""
	}
	if m.settings != nil {
		m.agent.Obs().SetEnabled(m.settings.Load().DebugLog)
	}
	run := fmt.Sprintf("tui-run-%d", atomic.AddInt32(&obsSeq, 1))
	m.agent.SetObsRun(m.cfg.WorkDir, "tui", run)
	m.agent.Obs().Log("info", "run_start", map[string]any{"prompt": prompt})
	return run
}

// obsEnd logs the run's completion and persists the message-level dump.
func (m *Model) obsEnd(run string, final []messages.Message, err error) {
	if m.agent == nil || m.agent.Obs() == nil || run == "" {
		return
	}
	data := map[string]any{"messages": len(final)}
	if err != nil {
		data["error"] = err.Error()
	}
	m.agent.Obs().Log("info", "run_end", data)
	m.agent.Obs().DumpMessages(final, map[string]any{"mode": "tui"})
}

// run starts the agent in a goroutine and returns a command that reads from
// the output channel.
func (m *Model) run() tea.Cmd {
	ch := make(chan tea.Msg, 512)
	m.ch = ch
	obsRun := m.obsBegin(m.lastPrompt)
	go func() {
		r := runner.New(m.agent, interruptHandler(ch), m.renderer(ch))
		result, err := r.Run(context.Background(), m.history)
		m.obsEnd(obsRun, result, err)
		ch <- runDone{result: result, err: err}
		close(ch)
	}()
	return readCmd(ch)
}

// runGoal starts the goal loop in a goroutine; its steps render exactly like
// a normal run (cycle headers and verdicts as deltas, tool use as chips).
func (m *Model) runGoal() tea.Cmd {
	ch := make(chan tea.Msg, 512)
	m.ch = ch
	obsRun := m.obsBegin("(goal loop)")
	go func() {
		r := runner.New(m.agent, interruptHandler(ch), m.renderer(ch))
		result, err := r.RunGoal(context.Background(), m.history)
		m.obsEnd(obsRun, result, err)
		ch <- runDone{result: result, err: err}
		close(ch)
	}()
	return readCmd(ch)
}

// maybeKickGoal starts the goal loop when a goal is active and the session is
// idle (after /goal set, /goal resume, or any finished run).
func (m *Model) maybeKickGoal() tea.Cmd {
	if m.running || m.agent == nil {
		return nil
	}
	g := m.agent.Goal()
	if g == nil || g.Status != agent.GoalActive || m.agent.GoalRunning() {
		return nil
	}
	m.write(dimStyle.Render("⏵ goal loop running (esc-free; /goal pause to stop at the next cycle)") + "\n")
	m.running = true
	return m.runGoal()
}

func readCmd(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// renderer converts graph steps into streamMsg increments, styled with
// colored tool-name chips, dimmed results, edit diffs, live todo
// snapshots, and an in-flight tool spinner tree.
func (m *Model) renderer(ch chan<- tea.Msg) runner.Renderer {
	running := map[string]string{} // toolCallID -> tool name
	renderMsg := func(msg messages.Message) {
		switch msg.Role {
		case messages.RoleAI:
			for _, tc := range msg.ToolCalls {
				running[tc.ID] = tc.Name
				ch <- streamMsg("\n" + toolColor(tc.Name).Render("⏺ "+tc.Name) + " " + dimStyle.Render(fmt.Sprintf("%v", tc.Args)) + "\n")
				if tc.Name == "Edit" || tc.Name == "Write" {
					for _, l := range diffLines(tc.Args) {
						ch <- streamMsg("  " + l + "\n")
					}
				}
			}
			ch <- inFlightMsg(sortedValues(running))
		case messages.RoleTool:
			delete(running, msg.ToolCallID)
			ch <- streamMsg(dimStyle.Render("  ⎿ "+summarize(msg.Content)) + "\n")
			if m.agent != nil {
				ch <- todosMsg(m.agent.Todos())
			}
			ch <- inFlightMsg(sortedValues(running))
		}
	}
	return func(chunk lggraph.StreamChunk) {
		switch chunk.Mode {
		case lggraph.StreamCustom:
			if s, ok := chunk.Payload.(string); ok && s != "" {
				ch <- streamMsg(s)
				return
			}
			// Goal cycles stream their tool use through the custom channel.
			if g, ok := chunk.Payload.(agent.GoalDelta); ok {
				renderMsg(g.Message)
				return
			}
		case lggraph.StreamUpdates:
			updates, _ := chunk.Payload.(map[string]any)
			for _, nodeUpdate := range updates {
				state, _ := nodeUpdate.(map[string]any)
				for _, msg := range agent.MessagesFrom(state) {
					renderMsg(msg)
				}
			}
		}
	}
}

// sortedValues returns the sorted values of a map.
func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// diffLines renders a tiny -/+ diff for Edit/Write tool arguments.
func diffLines(args map[string]any) []string {
	var out []string
	if old, ok := args["old_string"].(string); ok && old != "" {
		for _, l := range strings.Split(old, "\n") {
			out = append(out, delStyle.Render("- "+truncate(l, 120)))
		}
	}
	if new, ok := args["new_string"].(string); ok {
		for _, l := range strings.Split(new, "\n") {
			out = append(out, addStyle.Render("+ "+truncate(l, 120)))
		}
	}
	if len(out) == 0 {
		if content, ok := args["content"].(string); ok && content != "" {
			out = append(out, addStyle.Render("+ "+truncate(content, 120)))
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// padRight right-pads a string to width (no truncation).
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// resolvePendingKey handles a keypress while an interrupt awaits a decision.
func (m *Model) resolvePendingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if _, isAsk := m.pending.value.(agent.AskUserRequest); !isAsk {
		switch msg.String() {
		case "y", "Y", "enter":
			m.resolvePending("y")
		case "n", "N", "esc":
			m.resolvePending("n")
		}
		return m, nil
	}
	// AskUserQuestion: the input line collects the answer(s); enter submits
	// (multiple answers separated by ";").
	switch msg.String() {
	case "enter":
		answers := strings.Split(m.input, ";")
		for i := range answers {
			answers[i] = strings.TrimSpace(answers[i])
		}
		m.input = ""
		m.cursor = 0
		m.resolvePending(agent.AskUserAnswer{Answers: answers})
	case "backspace":
		if m.cursor > 0 {
			m.input = m.input[:m.cursor-1] + m.input[m.cursor:]
			m.cursor--
		}
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

// resolvePending answers the pending interrupt and echoes the decision into
// the transcript. The run goroutine unblocks and the runner resumes.
func (m *Model) resolvePending(answer any) {
	p := m.pending
	if p == nil {
		return
	}
	m.pending = nil
	switch v := p.value.(type) {
	case agent.PermissionRequest:
		verdict := "denied"
		if s, ok := answer.(string); ok && (s == "y" || s == "Y") {
			verdict = "allowed"
		}
		m.write("\n" + dimStyle.Render(fmt.Sprintf("[permission] %s — %s (%s by user)", v.ToolName, v.Reason, verdict)) + "\n")
	case agent.PlanApprovalRequest:
		verdict := "rejected"
		if s, ok := answer.(string); ok && (s == "y" || s == "Y") {
			verdict = "approved"
		}
		m.write("\n" + dimStyle.Render(fmt.Sprintf("[plan approval] (%s by user)", verdict)) + "\n")
	case agent.AskUserRequest:
		m.write("\n" + dimStyle.Render("[question] answered by user") + "\n")
	}
	p.answerCh <- answer
}

// pendingPrompt renders the plain-text body of the pending interrupt.
func (m *Model) pendingPrompt() string {
	switch v := m.pending.value.(type) {
	case agent.PermissionRequest:
		return fmt.Sprintf("Allow %s? — %s", v.ToolName, v.Reason)
	case agent.PlanApprovalRequest:
		return "Approve this plan?"
	case agent.AskUserRequest:
		var b strings.Builder
		for _, q := range v.Questions {
			b.WriteString(tools.FormatQuestion(0, q))
		}
		b.WriteString("Type your answer")
		if len(v.Questions) > 1 {
			b.WriteString("s, separated by ;")
		}
		b.WriteString(" and press enter")
		return b.String()
	}
	return "Continue?"
}

// interruptHandler surfaces run interrupts to the UI and blocks the run until
// the user answers; the runner then resumes via StreamResume.
func interruptHandler(ch chan<- tea.Msg) runner.InterruptHandler {
	return func(value any) (any, error) {
		answerCh := make(chan any, 1)
		ch <- interruptMsg{value: value, answerCh: answerCh}
		return <-answerCh, nil
	}
}

func summarize(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "(no output)"
	}
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		content = content[:idx]
	}
	if len(content) > 200 {
		content = content[:200] + "…"
	}
	return content
}

// goalFooter renders the footer's goal segment: cycle/token budget (and the
// pending completion claim, if any).
func goalFooter(g *agent.Goal) string {
	s := fmt.Sprintf("cycle %d/%d · %s/%s tok", g.Cycles, g.MaxCycles, agent.FormatGoalTokens(g.TokensUsed), agent.FormatGoalTokens(g.MaxTokens))
	if g.Claimed {
		s += " · claim pending verification"
	}
	return s
}

// write appends a fragment, splitting complete lines into the transcript.
func (m *Model) write(s string) {
	m.cur += s
	for {
		idx := strings.IndexByte(m.cur, '\n')
		if idx < 0 {
			break
		}
		m.append(m.cur[:idx])
		m.cur = m.cur[idx+1:]
	}
}

// flush moves any trailing fragment into the transcript.
func (m *Model) flush() {
	if m.cur != "" {
		m.append(m.cur)
		m.cur = ""
	}
}

func (m *Model) append(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > 500 {
		m.lines = m.lines[len(m.lines)-500:]
	}
	m.viewport.SetLines(m.lines)
}

// View renders the transcript (through the viewport), a todo panel, the
// prompt box, and a footer status line.
func (m *Model) View() string {
	var b strings.Builder

	if len(m.lines) == 0 {
		for _, line := range m.bannerLines() {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(m.todos) > 0 {
		b.WriteString(boldDimStyle.Render("✻ Todo list") + "\n")
		for _, t := range m.todos {
			mark := " "
			switch t.Status {
			case "in_progress":
				mark = ">"
			case "completed":
				mark = "x"
			case "cancelled":
				mark = "-"
			}
			text := t.Content
			if t.ActiveForm != "" {
				text = t.ActiveForm
			}
			b.WriteString(dimStyle.Render("  ["+mark+"] "+text) + "\n")
		}
		b.WriteString("\n")
	}

	for _, line := range m.viewport.Visible() {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if m.running || len(m.inFlight) > 0 {
		frame := m.spinner.View()
		if len(m.inFlight) > 0 {
			for _, name := range m.inFlight {
				b.WriteString(dimStyle.Render(frame+" "+name) + "\n")
			}
		} else {
			b.WriteString(dimStyle.Render(frame+" thinking…") + "\n")
		}
	}
	if m.pending != nil {
		title := boldDimStyle.Render("⏵ awaiting your decision")
		body := m.pendingPrompt()
		var hint string
		if _, isAsk := m.pending.value.(agent.AskUserRequest); isAsk {
			hint = m.renderInput()
		} else {
			hint = dimStyle.Render("y = allow · enter = allow · n / esc = deny")
		}
		b.WriteString(boxStyle.Render(title + "\n" + body + "\n" + hint))
		b.WriteString("\n")
	}
	content := promptStyle.Render("❯ ") + m.renderInput()
	hints := dimStyle.Render("! for bash mode · / for commands · esc to interrupt · ctrl-c to exit")
	b.WriteString(boxStyle.Render(content + "\n" + hints))
	b.WriteString("\n")
	const col = 18 // command-name column width
	for i, c := range suggestions(m.input) {
		name := padRight(c.name, col)
		if i == m.sel {
			b.WriteString(selStyle.Render("  "+name) + selDescStyle.Render("  "+c.desc) + "\n")
		} else {
			b.WriteString(dimStyle.Render("  "+name+"  "+c.desc) + "\n")
		}
	}
	model, mode := m.cfg.Model, m.cfg.PermissionMode
	if m.console != nil {
		live := m.console.Cfg()
		model, mode = live.Model, live.PermissionMode
	}
	status := fmt.Sprintf("model=%s · mode=%s", model, mode)
	if m.agent != nil {
		if g := m.agent.Goal(); g != nil {
			status += fmt.Sprintf(" · goal[%s] %s", g.Status, goalFooter(g))
		}
	}
	if VimMode() {
		if m.vimInsert {
			status += " · vim: INSERT"
		} else {
			status += " · vim: NORMAL"
		}
	}
	b.WriteString(dimStyle.Render(status))
	b.WriteString("\n")
	return b.String()
}

// renderInput renders the input with a block cursor at m.cursor.
func (m *Model) renderInput() string {
	if m.cursor >= len(m.input) {
		return m.input + cursorStyle.Render(" ")
	}
	return m.input[:m.cursor] + cursorStyle.Render(m.input[m.cursor:m.cursor+1]) + m.input[m.cursor+1:]
}

// vimKey handles one keystroke in vim normal mode. It reports whether the
// key was consumed; enter and unrecognized printable keys fall through to
// the caller's standard handling.
func (m *Model) vimKey(msg tea.KeyMsg) bool {
	s := msg.String()
	if m.vimPendingD && s != "d" {
		m.vimPendingD = false
	}
	switch s {
	case "i":
		m.vimInsert = true
	case "a":
		m.vimInsert = true
		if m.cursor < len(m.input) {
			m.cursor++
		}
	case "A":
		m.vimInsert = true
		m.cursor = len(m.input)
	case "h":
		if m.cursor > 0 {
			m.cursor--
		}
	case "l":
		if m.cursor < len(m.input) {
			m.cursor++
		}
	case "0":
		m.cursor = 0
	case "$":
		m.cursor = len(m.input)
	case "w":
		m.cursor = vimWordForward(m.input, m.cursor)
	case "b":
		m.cursor = vimWordBackward(m.input, m.cursor)
	case "x":
		if m.cursor < len(m.input) {
			m.input = m.input[:m.cursor] + m.input[m.cursor+1:]
		}
	case "D":
		m.input = m.input[:m.cursor]
	case "d":
		if m.vimPendingD {
			m.input = ""
			m.cursor = 0
			m.vimPendingD = false
		} else {
			m.vimPendingD = true
		}
	case "esc":
		// already normal mode
	case "backspace":
		if m.cursor > 0 {
			m.cursor--
		}
	case "delete":
		if m.cursor < len(m.input) {
			m.input = m.input[:m.cursor] + m.input[m.cursor+1:]
		}
	default:
		// Unknown single runes are consumed silently (normal mode never
		// types); multi-key combos (ctrl/arrow/enter) fall through to the
		// standard handling.
		return len(msg.Runes) == 1 && msg.Type == tea.KeyRunes
	}
	return true
}

// vimWordForward moves the cursor to the start of the next word.
func vimWordForward(s string, pos int) int {
	i := pos
	for i < len(s) && !isWordBreak(s[i]) {
		i++
	}
	for i < len(s) && isWordBreak(s[i]) {
		i++
	}
	return i
}

// vimWordBackward moves the cursor to the start of the previous word.
func vimWordBackward(s string, pos int) int {
	i := pos
	for i > 0 && isWordBreak(s[i-1]) {
		i--
	}
	for i > 0 && !isWordBreak(s[i-1]) {
		i--
	}
	return i
}

func isWordBreak(c byte) bool { return c == ' ' || c == '\t' }

func stripSystem(msgs []messages.Message) []messages.Message {
	if len(msgs) > 0 && msgs[0].Role == messages.RoleSystem {
		return msgs[1:]
	}
	return msgs
}

// handleCommand processes slash commands; returns (true, cmd) when the input
// was a command (so it is not sent to the agent) — cmd may start a run when
// the command hands back a prompt (/init). Returns (false, nil) for plain
// prompts.
func (m *Model) handleCommand(text string) (bool, tea.Cmd) {
	if m.console == nil {
		if text == "/help" {
			m.write("Commands: /help /exit /quit (attach a console for the full set)\n")
			return true, nil
		}
		return false, nil
	}
	parts := strings.Fields(strings.TrimPrefix(text, "/"))
	if len(parts) == 0 {
		return false, nil
	}
	session := &console.Session{History: m.history}
	res, err := m.console.Execute(context.Background(), parts[0], parts[1:], session)
	if err != nil {
		m.write("Error: " + err.Error() + "\n")
		return true, nil
	}
	m.history = session.History
	if res.Clear {
		m.lines = nil
		m.cur = ""
		m.viewport.SetLines(nil)
	}
	if res.Output != "" {
		m.write(res.Output)
	}
	if res.Prompt != "" {
		return true, m.submit(res.Prompt)
	}
	// /goal set and /goal resume (or any command, really) may have left an
	// active idle goal — kick the loop.
	return true, m.maybeKickGoal()
}

// submit appends a user message and starts one agent run.
func (m *Model) submit(text string) tea.Cmd {
	m.write(userStyle.Render("❯ "+text) + "\n")
	m.history = append(m.history, messages.Human(text))
	m.running = true
	m.lastPrompt = text
	return m.run()
}

// command is a slash command with its description.
type command struct {
	name string
	desc string
}

// slashCommands is the completion table, derived from the console command
// registry so the two never drift.
var slashCommands = func() []command {
	var out []command
	for _, c := range console.Commands() {
		out = append(out, command{name: "/" + c.Name, desc: c.Description})
	}
	return out
}()

// suggestions returns slash commands matching the current input prefix.
func suggestions(input string) []command {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	prefix := strings.ToLower(input)
	var out []command
	for _, c := range slashCommands {
		if strings.HasPrefix(strings.ToLower(c.name), prefix) {
			out = append(out, c)
		}
	}
	return out
}

// Option customizes the TUI at startup.
type Option func(*Model)

// WithConsole attaches the shared console command layer.
func WithConsole(c *console.Console) Option {
	return func(m *Model) { m.console = c }
}

// WithSettingsStore enables the per-run debug-log dynamic refresh (toggles
// made in other processes apply to this TUI on its next run).
func WithSettingsStore(st *settings.Store) Option {
	return func(m *Model) { m.settings = st }
}

// Run starts the TUI program and blocks until it exits.
func Run(a *agent.Agent, history []messages.Message, cfg config.Config, version string, opts ...Option) error {
	m := New(a, history, cfg)
	if version != "" {
		m.version = version
	}
	for _, opt := range opts {
		opt(m)
	}
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
