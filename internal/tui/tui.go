// Package tui provides a bubbletea terminal interface for the woca agent: a
// scrolling transcript that updates live as the model streams, and an input
// line.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/projanvil/langchain-golang/core/messages"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/keybindings"
	"github.com/projanvil/woca/internal/notify"
	"github.com/projanvil/woca/internal/promptref"
	"github.com/projanvil/woca/internal/repl"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
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

// streamMsg carries one increment of live output.
type streamMsg string

// toolCallMsg carries one tool invocation (live rendering + review blocks).
type toolCallMsg struct {
	id   string
	name string
	args map[string]any
}

// toolResultMsg carries one tool result.
type toolResultMsg struct {
	id      string
	content string
}

// reasoningMsg carries one reasoning (chain-of-thought) increment.
type reasoningMsg struct {
	text string
}

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
		dimStyle.Render("  ! for bash mode · esc-esc to interrupt · enter queues the next message while running"),
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
	// cancelRun aborts the in-flight run goroutine (double-esc or ctrl+c
	// while running). Nil when the session is idle.
	cancelRun context.CancelFunc
	// interrupting marks a user-requested cancel so the resulting
	// context.Canceled error renders as a marker instead of a failure.
	interrupting bool
	// lastEsc timestamps the previous esc press; a second esc inside
	// escDoubleWindow interrupts the running turn (same semantics and
	// threshold as the web UI).
	lastEsc time.Time
	// lastCtrlC arms the idle two-press exit window.
	lastCtrlC time.Time
	// hint is a transient footer message (cleared by the next keypress).
	hint string
	// queued parks one message typed while a run is in flight — single
	// slot, last-write-wins, flushed automatically the moment the run goes
	// idle (web useMessageQueue parity).
	queued *queuedMsg
	// draftHeld/draft preserve the composer line and cursor while a
	// decision box owns the keyboard.
	draftHeld bool
	draft     string
	// decSel is the highlighted allow/deny cell for permission and plan
	// approvals (0 = allow, 1 = deny, the safe default).
	decSel int
	// askIdx/askSel/askPicks/askAnswers drive the sequential AskUserQuestion
	// machine: one question at a time, options as a selectable list.
	askIdx     int
	askSel     int
	askPicks   []bool
	askAnswers []string
	// blocks is the structured transcript behind the review overlay
	// (ctrl+o): full tool args/results, reasoning, prompts, and command
	// output, untruncated.
	blocks []block
	// overlay is the open full-screen layer (review, palette, sessions);
	// esc pops back to the base view, which keeps updating underneath.
	overlay overlayKind
	// review state: selected block, fold flags (absent = default folded),
	// and the scroll offset.
	reviewSel    int
	reviewFolded map[int]bool
	reviewScroll int
	// palette state (ctrl+k): filter text and highlighted entry.
	paletteFilter string
	paletteSel    int
	// sessions state (ctrl+r): filter text, highlighted entry, and the
	// snapshot list loaded when the picker opened.
	sessionsFilter  string
	sessionsSel     int
	sessionsEntries []console.SessionEntry
	// models state (bare /model): filter text, highlighted entry, and the
	// catalog list loaded when the picker opened.
	modelsFilter  string
	modelsSel     int
	modelsEntries []console.ModelEntry
	// completion inventories (skills for "/", subagents for "@"),
	// refreshed after every submitted turn.
	skillList []skills.Skill
	agentList []agents.Definition
	// ctxInfo is the post-run context-usage snapshot for the status bar.
	ctxInfo *agent.ContextInfo
	// keys resolves composer key presses to actions, defaults overridden by
	// ~/.woca/keybindings.json when present (see internal/keybindings).
	keys keybindings.Map
}

// New creates the TUI model.
func New(a *agent.Agent, history []messages.Message, cfg config.Config) *Model {
	keys := keybindings.Default()
	if path, err := keybindings.DefaultPath(); err == nil {
		keys, _ = keybindings.Load(path)
	}
	return &Model{
		agent:        a,
		history:      history,
		cfg:          cfg,
		version:      "0.3.0",
		viewport:     NewViewport(),
		height:       24,
		spinner:      spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		reviewFolded: map[int]bool{},
		keys:         keys,
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
		// An open overlay owns the keyboard next; unconsumed keys fall
		// through to the base handler.
		if consumed, cmd := m.overlayKey(msg); consumed {
			return m, cmd
		}
		// Vim modal input (/vim): in normal mode keys edit,
		// enter still submits. Interception happens before the standard
		// handling; enter falls through to it.
		if VimMode() && !m.vimInsert && !m.running {
			if handled := m.vimKey(msg); handled {
				return m, nil
			}
		}
		// Composer actions resolve through the (possibly user-overridden)
		// keybinding map before the default, fixed key handling below.
		switch m.keys.Action(msg.String()) {
		case keybindings.ActionComposerNewline:
			m.input = m.input[:m.cursor] + "\n" + m.input[m.cursor:]
			m.cursor++
			m.sel = 0
			return m, nil
		case keybindings.ActionComposerEditor:
			if cmd := m.openEditorCmd(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+d":
			return m, tea.Quit
		case "ctrl+c":
			// While a turn runs, ctrl+c interrupts it (same as double-esc).
			// Idle: first press clears the draft, second press inside the
			// window exits.
			if m.running {
				m.interrupt()
				return m, nil
			}
			now := time.Now()
			if m.input != "" {
				m.input = ""
				m.cursor = 0
				m.sel = 0
				m.hint = ""
				return m, nil
			}
			if now.Sub(m.lastCtrlC) < ctrlCExitWindow {
				return m, tea.Quit
			}
			m.lastCtrlC = now
			m.hint = "ctrl+c again to exit"
			return m, nil
		case "esc":
			m.hint = ""
			if VimMode() && m.vimInsert {
				m.vimInsert = false
			}
			// Double-esc inside the window interrupts the running turn (the
			// single first press only arms; idle presses do nothing).
			now := time.Now()
			if m.running && now.Sub(m.lastEsc) < escDoubleWindow {
				m.interrupt()
				m.lastEsc = time.Time{}
			} else {
				m.lastEsc = now
			}
		case "ctrl+a":
			m.cursor = 0
		case "ctrl+e":
			m.cursor = len(m.input)
		case "pgup":
			m.viewport.PageUp()
		case "pgdown":
			m.viewport.PageDown()
		case "ctrl+o":
			m.openReview()
			return m, nil
		case "ctrl+k":
			m.openPalette()
			return m, nil
		case "ctrl+r":
			m.openSessions()
			return m, nil
		case "up":
			// An empty line recalls the parked next message for editing
			// before falling through to history navigation.
			if m.input == "" && m.queued != nil {
				m.input = m.queued.text
				m.cursor = len(m.input)
				m.queued = nil
				return m, nil
			}
			if cmds := m.allSuggestions(m.input); len(cmds) > 0 {
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
			if cmds := m.allSuggestions(m.input); len(cmds) > 0 {
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
			if strings.HasPrefix(m.input, "/") || atTrigger(m.input) >= 0 {
				if cmds := m.allSuggestions(m.input); len(cmds) > 0 {
					if m.sel >= len(cmds) {
						m.sel = len(cmds) - 1
					}
					m.acceptCompletion(cmds[m.sel].name)
				}
			}
		case "ctrl+u":
			m.input = ""
			m.cursor = 0
			m.sel = 0
		case "left":
			if m.cursor > 0 {
				m.cursor--
			}
		case "right":
			if m.cursor < len(m.input) {
				m.cursor++
			}
		case "enter":
			// While a turn runs, enter parks the line as the next message
			// instead of swallowing it.
			if m.running {
				m.park()
				return m, nil
			}
			// In a completion menu (commands, skills, or @agents), Enter
			// first accepts the highlighted entry; a second Enter submits.
			if m.completionsMenuVisible() {
				cmds := m.allSuggestions(m.input)
				sel := m.sel
				if sel >= len(cmds) {
					sel = len(cmds) - 1
				}
				entry := cmds[sel].name
				if strings.TrimSpace(m.input) != entry {
					m.acceptCompletion(entry)
					return m, nil
				}
			}
			text := strings.TrimSpace(m.input)
			m.input = ""
			m.cursor = 0
			if text == "" {
				return m, nil
			}
			m.remember(text)
			return m, m.dispatch(text)
		case "backspace":
			if m.cursor > 0 {
				m.input = m.input[:m.cursor-1] + m.input[m.cursor:]
				m.cursor--
			}
		case "delete":
			if m.cursor < len(m.input) {
				m.input = m.input[:m.cursor] + m.input[m.cursor+1:]
			}
		default:
			if len(msg.Runes) > 0 {
				r := string(msg.Runes)
				m.input = m.input[:m.cursor] + r + m.input[m.cursor:]
				m.cursor += len(msg.Runes)
				m.sel = 0
				m.hint = ""
			}
		}
	case editorResult:
		m.applyEditorResult(msg)
	case streamMsg:
		m.appendStreamText(blockAssistant, string(msg))
		m.write(string(msg))
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case reasoningMsg:
		m.appendStreamText(blockReasoning, msg.text)
		m.write(dimStyle.Render(msg.text))
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case toolCallMsg:
		m.appendToolCall(msg.id, msg.name, msg.args)
		m.write("\n" + toolColor(msg.name).Render("⏺ "+msg.name) + " " + dimStyle.Render(fmt.Sprintf("%v", msg.args)) + "\n")
		if msg.name == "Edit" || msg.name == "Write" {
			for _, l := range diffLines(msg.args) {
				m.write("  " + l + "\n")
			}
		}
		if m.ch != nil {
			return m, readCmd(m.ch)
		}
	case toolResultMsg:
		m.attachToolResult(msg.id, msg.content)
		m.write(dimStyle.Render("  ⎿ "+summarize(msg.content)) + "\n")
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
		// A run is paused awaiting this decision; park the composer draft so
		// the decision keys can never swallow typing in progress, render the
		// approval UI, and keep draining the run channel while the user
		// decides.
		m.holdDraft()
		m.pending = &msg
		m.pendingBegin()
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
		m.cancelRun = nil
		wasInterrupted := m.interrupting
		m.interrupting = false
		m.flush()
		m.ch = nil
		if msg.err != nil {
			if wasInterrupted || errors.Is(msg.err, context.Canceled) {
				m.systemLine(dimStyle.Render("⏹ interrupted by user") + "\n")
			} else {
				m.systemLine("Error: " + msg.err.Error() + "\n")
				notify.Attention(os.Stdout, "woca: run failed")
			}
		} else {
			m.history = stripSystem(msg.result)
			notify.Attention(os.Stdout, "woca: response ready")
		}
		if m.agent != nil {
			m.todos = m.agent.Todos()
		}
		m.refreshCompletions()
		m.refreshContext()
		// The parked next message flushes the moment the run goes idle —
		// after a natural completion or an interrupt alike (web parity).
		if q := m.queued; q != nil {
			m.queued = nil
			m.remember(q.text)
			return m, m.dispatch(q.text)
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
// the output channel. The run is cancelable: double-esc or ctrl+c cancels
// the context, which unwinds the graph and ends the turn.
func (m *Model) run() tea.Cmd {
	return m.runWithHistory(m.history)
}

func (m *Model) runWithHistory(history []messages.Message) tea.Cmd {
	ch := make(chan tea.Msg, 512)
	m.ch = ch
	obsRun := m.obsBegin(m.lastPrompt)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel
	go func() {
		r := runner.New(m.agent, interruptHandler(ch), m.renderer(ch))
		result, err := r.Run(ctx, history)
		m.obsEnd(obsRun, result, err)
		ch <- runDone{result: result, err: err}
		close(ch)
	}()
	return readCmd(ch)
}

// runGoal starts the goal loop in a goroutine; its steps render exactly like
// a normal run (cycle headers and verdicts as deltas, tool use as chips).
// Double-esc interrupts the in-flight cycle; the loop then stays idle until
// /goal resume.
func (m *Model) runGoal() tea.Cmd {
	ch := make(chan tea.Msg, 512)
	m.ch = ch
	obsRun := m.obsBegin("(goal loop)")
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel
	go func() {
		r := runner.New(m.agent, interruptHandler(ch), m.renderer(ch))
		result, err := r.RunGoal(ctx, m.history)
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
	m.write(dimStyle.Render("⏵ goal loop running (esc-esc interrupts the current cycle; /goal pause to stop at the next cycle)") + "\n")
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

// renderer converts graph steps into structured messages (stream deltas,
// reasoning, tool calls and results); Update renders the live styled lines
// from them and records the review blocks, keeping the two views from ever
// drifting apart.
func (m *Model) renderer(ch chan<- tea.Msg) runner.Renderer {
	running := map[string]string{} // toolCallID -> tool name
	renderMsg := func(msg messages.Message) {
		switch msg.Role {
		case messages.RoleAI:
			for _, tc := range msg.ToolCalls {
				running[tc.ID] = tc.Name
				ch <- toolCallMsg{id: tc.ID, name: tc.Name, args: tc.Args}
			}
			ch <- inFlightMsg(sortedValues(running))
		case messages.RoleTool:
			delete(running, msg.ToolCallID)
			ch <- toolResultMsg{id: msg.ToolCallID, content: msg.Content}
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
			switch p := chunk.Payload.(type) {
			// Reasoning streams dimmed, in full, as it arrives.
			case agent.ReasoningDelta:
				if p.Text != "" {
					ch <- reasoningMsg{text: p.Text}
				}
			// Goal cycles stream their tool use through the custom channel.
			case agent.GoalDelta:
				renderMsg(p.Message)
			// Goal-loop narration renders dimmed: cycle banners and the
			// executor's per-cycle chatter are progress, not the answer.
			case agent.GoalNote:
				if p.Text != "" {
					ch <- streamMsg(dimStyle.Render(p.Text))
				}
			// The goal run's single unified reply renders as the answer.
			case agent.GoalFinalReply:
				if p.Text != "" {
					ch <- streamMsg(p.Text)
				}
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

// resolvePending answers the pending interrupt and echoes the decision into
// the transcript. The run goroutine unblocks and the runner resumes. Any
// held composer draft returns to the input line.
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
		answers := ""
		if aa, ok := answer.(agent.AskUserAnswer); ok {
			answers = strings.Join(aa.Answers, " · ")
		}
		m.write("\n" + dimStyle.Render("[question] answered by user: "+answers) + "\n")
	}
	m.restoreDraft()
	p.answerCh <- answer
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

// systemLine writes a system fragment (command output, errors, markers)
// into both the live transcript and the review blocks.
func (m *Model) systemLine(s string) {
	m.appendSystemBlock(s)
	m.write(s)
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

// transcriptLimit bounds the live transcript lines (long sessions scroll the
// oldest display out; the structured blocks keep a deeper untruncated copy).
const transcriptLimit = 5000

func (m *Model) append(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > transcriptLimit {
		m.lines = m.lines[len(m.lines)-transcriptLimit:]
	}
	m.viewport.SetLines(m.lines)
}

// View renders the transcript (through the viewport), a todo panel, the
// prompt box, and a footer status line. An open overlay replaces the whole
// screen; the base view keeps updating underneath through Update.
func (m *Model) View() string {
	if m.overlay != overlayNone {
		return m.overlayView()
	}
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
		if ask, isAsk := m.pending.value.(agent.AskUserRequest); isAsk {
			if m.askIdx < len(ask.Questions) && len(ask.Questions[m.askIdx].Options) > 0 {
				if ask.Questions[m.askIdx].MultiSelect {
					hint = dimStyle.Render("↑↓ move · space toggle · enter confirm · esc skip")
				} else {
					hint = dimStyle.Render("↑↓ select · enter confirm · esc skip")
				}
			} else {
				hint = m.renderInput()
			}
		} else {
			hint = dimStyle.Render("←→ select · enter = highlighted (deny) · y allow · n / esc deny")
		}
		if m.draftHeld {
			body += dimStyle.Render("draft (restored after): "+m.draft) + "\n"
		}
		b.WriteString(boxStyle.Render(title + "\n" + body + hint))
		b.WriteString("\n")
	}
	if m.queued != nil {
		b.WriteString(dimStyle.Render("⏳ next: "+summarize(m.queued.text)) + dimStyle.Render(" · ↑ to edit") + "\n")
	}
	content := promptStyle.Render("❯ ") + m.renderInput()
	hints := "! bash · / commands & skills · @ agents · pgup/pgdn scroll"
	if m.running {
		hints = "esc-esc interrupt · enter queue next message · pgup/pgdn scroll"
	}
	if m.hint != "" {
		hints = m.hint
	}
	b.WriteString(boxStyle.Render(content + "\n" + dimStyle.Render(hints)))
	b.WriteString("\n")
	const col = 18 // entry-name column width
	menu := m.allSuggestions(m.input)
	lastGroup := ""
	for i, c := range menu {
		if c.group != "" && c.group != lastGroup {
			lastGroup = c.group
			b.WriteString(boldDimStyle.Render("  "+c.group) + "\n")
		}
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
	if seg := ctxFooter(m.ctxInfo); seg != "" {
		// The ctx segment styles itself (semantic threshold colors), so it
		// joins after the dim render instead of under it.
		status = dimStyle.Render(status) + " · " + seg
	} else {
		status = dimStyle.Render(status)
	}
	b.WriteString(status)
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

// editorResult carries the outcome of a `ctrl+g` $VISUAL/$EDITOR session.
type editorResult struct {
	content string
	err     error
}

// openEditorCmd opens the current draft in $VISUAL (falling back to
// $EDITOR) via a suspended subprocess. It returns nil, setting a
// non-destructive hint, when neither variable is set or the draft cannot be
// staged to a temp file.
func (m *Model) openEditorCmd() tea.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		m.hint = "set $VISUAL or $EDITOR to edit the draft externally"
		return nil
	}
	f, err := os.CreateTemp("", "woca-draft-*.md")
	if err != nil {
		m.hint = "ctrl+g: " + err.Error()
		return nil
	}
	path := f.Name()
	if _, err := f.WriteString(m.input); err != nil {
		f.Close()
		os.Remove(path)
		m.hint = "ctrl+g: " + err.Error()
		return nil
	}
	f.Close()
	cmd := exec.Command(editor, path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editorResult{err: err}
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return editorResult{err: rerr}
		}
		return editorResult{content: strings.TrimSuffix(string(content), "\n")}
	})
}

// applyEditorResult replaces the draft with the editor's output on success;
// on failure the original draft is untouched and the error surfaces as a
// hint.
func (m *Model) applyEditorResult(r editorResult) {
	if r.err != nil {
		m.hint = "ctrl+g: " + r.err.Error()
		return
	}
	m.input = r.content
	m.cursor = len(m.input)
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
	if !strings.HasPrefix(text, "/") {
		return false, nil
	}
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
		m.systemLine("Error: " + err.Error() + "\n")
		return true, nil
	}
	m.history = session.History
	if res.Clear {
		m.lines = nil
		m.cur = ""
		m.blocks = nil
		m.reviewFolded = map[int]bool{}
		m.viewport.SetLines(nil)
	}
	if res.Output != "" {
		m.systemLine(res.Output)
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
	runHistory, err := m.historyForPrompt(text)
	if err != nil {
		m.input = text
		m.cursor = len(text)
		m.systemLine("Error: " + err.Error() + "\n")
		return nil
	}
	m.write(userStyle.Render("❯ "+text) + "\n")
	m.appendUserBlock(text)
	m.history = append(m.history, messages.Human(text))
	m.running = true
	m.lastPrompt = text
	m.refreshCompletions()
	return m.runWithHistory(runHistory)
}

func (m *Model) historyForRun() ([]messages.Message, error) {
	if len(m.history) == 0 {
		return nil, nil
	}
	last := len(m.history) - 1
	if m.history[last].Role != messages.RoleHuman {
		return append([]messages.Message(nil), m.history...), nil
	}
	base := append([]messages.Message(nil), m.history[:last]...)
	return m.historyForPromptWithBase(base, m.history[last].Content)
}

func (m *Model) historyForPrompt(text string) ([]messages.Message, error) {
	return m.historyForPromptWithBase(m.history, text)
}

func (m *Model) historyForPromptWithBase(base []messages.Message, text string) ([]messages.Message, error) {
	expanded, err := promptref.Expand(text, m.cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	history := append([]messages.Message(nil), base...)
	history = append(history, messages.Human(expanded))
	return history, nil
}

// dispatch routes one submitted line exactly like the line REPL classifies
// them (repl.Parse): `!command` runs in the shell and feeds the output to
// the conversation, `/name` runs a console command, a `/skill-name` line
// that matches a loaded skill becomes a model turn (web parity — the Skill
// tool owns it), and everything else is a plain prompt.
func (m *Model) dispatch(text string) tea.Cmd {
	switch a := repl.Parse(text); a.Kind {
	case repl.KindQuit:
		return tea.Quit
	case repl.KindShell:
		m.runShellLine(a.Text)
		return nil
	case repl.KindCommand:
		// A bare /model with a catalog opens the picker instead of the
		// text listing (the line REPL keeps the listing).
		if a.Text == "model" && len(a.Args) == 0 && m.hasModelCatalog() {
			m.openModels()
			return nil
		}
		if m.knownCommand(a.Text) {
			_, cmd := m.handleCommand(text)
			return cmd
		}
		if m.isSkillName(a.Text) {
			return m.submit(text)
		}
		m.systemLine("Error: unknown command /" + a.Text + " (try /help)\n")
		return nil
	}
	return m.submit(text)
}

// knownCommand reports whether name is in the console command table.
func (m *Model) knownCommand(name string) bool {
	if m.console == nil {
		return name == "help"
	}
	for _, c := range console.Commands() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// isSkillName reports whether name matches a loaded skill, making a
// `/skill-name` line a Skill-tool turn instead of a console command.
func (m *Model) isSkillName(name string) bool {
	if m.agent == nil {
		return false
	}
	for _, s := range m.agent.SkillsIn(context.Background()) {
		if s.Name == name {
			return true
		}
	}
	return false
}

// runShellLine executes one `!command` line, echoes the output into the
// transcript, and feeds it to the conversation (the model sees the same
// [shell] message shape as the line REPL).
func (m *Model) runShellLine(command string) {
	out := repl.RunShell(command)
	m.systemLine("$ " + command + "\n" + out + "\n")
	m.history = append(m.history, messages.Human(repl.ShellFeed(command, out)))
}

// remember appends a submitted line to the input history (deduplicated).
func (m *Model) remember(text string) {
	if len(m.inputHist) == 0 || m.inputHist[len(m.inputHist)-1] != text {
		m.inputHist = append(m.inputHist, text)
	}
	m.histIdx = len(m.inputHist)
}

// command is one completion entry with its description and menu group
// (commands, skills, or agents).
type command struct {
	name  string
	desc  string
	group string
}

// slashCommands is the completion table, derived from the console command
// registry so the two never drift.
var slashCommands = func() []command {
	var out []command
	for _, c := range console.Commands() {
		out = append(out, command{name: "/" + c.Name, desc: c.Description, group: groupCommands})
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
