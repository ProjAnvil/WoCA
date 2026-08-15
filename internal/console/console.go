// Package console implements the interactive console's slash commands, shared
// by the bubbletea TUI and the line-based REPL — a miniature of Claude Code's
// command registry (commands.ts) covering session control (/clear, /compact),
// introspection (/status, /context, /todos, /tasks, /skills, /mcp), runtime
// configuration (/model, /permissions), and the on-demand web GUI (/web).
package console

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/slashcmds"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tokens"
	"github.com/projanvil/woca/internal/webgui"
)

// Session is the mutable conversation state commands operate on.
type Session struct {
	// History is the current conversation (without the system prompt).
	History []messages.Message
}

// Result is the outcome of a console command.
type Result struct {
	// Output is text to print to the transcript (empty = print nothing).
	Output string
	// Exit terminates the console loop.
	Exit bool
	// Clear asks the caller to clear the transcript display (used by /clear).
	Clear bool
	// Prompt, when non-empty, is submitted to the agent as a user turn right
	// after the command (used by /init, which drives a full analysis run).
	Prompt string
}

// Command describes one slash command for help and completion.
type Command struct {
	Name        string
	Description string
}

// Deps wires the services console commands act on. Nil members disable the
// corresponding commands gracefully.
type Deps struct {
	// Agent is the coding agent (required).
	Agent *agent.Agent
	// Cfg is the agent configuration; /model updates it in place.
	Cfg *config.Config
	// Web starts/stops the embedded web GUI on demand (/web). Nil disables.
	Web *webgui.Manager
	// ModelFactory rebuilds a chat model by name for /model switching. Nil
	// disables switching (viewing still works).
	ModelFactory func(name string) (language.ChatModel, error)
	// Version is the woca version shown by /status.
	Version string
	// OpenBrowser opens a URL in the default browser (best-effort, optional).
	OpenBrowser func(url string)
	// Settings persists runtime configuration (/settings). Nil disables the
	// command with a helpful message.
	Settings *settings.Store
	// Memory is the memoryStore (/memory). Nil disables the command.
	Memory *store.Store
	// RebuildModel rebuilds the chat model after provider/credential changes
	// made via /settings (optional).
	RebuildModel func() error
	// SessionDir overrides where /branch writes session snapshots (tests);
	// empty uses ~/.woca/sessions.
	SessionDir string
	// AgentsDir overrides where /agents new writes definitions (tests);
	// empty uses ~/.woca/agents.
	AgentsDir string
	// PluginsRoot overrides the plugin installation root (tests); empty
	// uses ~/.woca/plugins.
	PluginsRoot string
	// ApplyTheme switches the TUI palette live (/theme); nil disables the
	// command with a helpful message.
	ApplyTheme func(name string) error
	// SetVimModeToggle flips the TUI vim input mode and reports the new
	// state (/vim); nil disables the command.
	SetVimModeToggle func() bool
}

// Console executes slash commands against a Deps set.
type Console struct {
	deps Deps
}

// New creates a Console. A nil Cfg is tolerated (/model falls back to
// read-only).
func New(deps Deps) *Console {
	if deps.Cfg == nil {
		deps.Cfg = &config.Config{}
	}
	return &Console{deps: deps}
}

// Cfg returns the live configuration (updated by /model).
func (c *Console) Cfg() config.Config { return *c.deps.Cfg }

// Commands returns the command table sorted by name. The builtin table is
// shared with the web server's autocomplete via internal/slashcmds.
func Commands() []Command {
	builtin := slashcmds.Builtin()
	list := make([]Command, len(builtin))
	for i, c := range builtin {
		list[i] = Command{Name: c.Name, Description: c.Description}
	}
	return list
}

// Execute runs a command by name with its arguments. Unknown commands return
// an error the caller prints.
func (c *Console) Execute(ctx context.Context, name string, args []string, session *Session) (Result, error) {
	switch name {
	case "help":
		return Result{Output: c.helpText()}, nil
	case "status", "config":
		return Result{Output: c.status()}, nil
	case "permissions":
		return c.permissions(args)
	case "model":
		return c.model(args)
	case "clear":
		session.History = nil
		return Result{Output: "conversation cleared\n", Clear: true}, nil
	case "compact":
		return c.compact(ctx, session)
	case "context":
		return Result{Output: c.context(session)}, nil
	case "cost":
		return Result{Output: c.cost()}, nil
	case "init":
		return c.initCmd(session)
	case "rewind":
		return c.rewind(args, session)
	case "branch":
		return c.branch(args, session)
	case "doctor":
		return c.doctorCmd(), nil
	case "export":
		return c.exportCmd(args, session)
	case "rename":
		return c.renameCmd(args, session)
	case "agents":
		return c.agentsCmd(args)
	case "output-style":
		return c.outputStyleCmd(args)
	case "plugins":
		return c.pluginsCmd(args)
	case "theme":
		return c.themeCmd(args)
	case "vim":
		return c.vimCmd(), nil
	case "keybindings":
		return c.keybindingsCmd(), nil
	case "todos":
		return Result{Output: c.todos()}, nil
	case "goal":
		return c.goalCmd(args)
	case "tasks":
		return Result{Output: c.tasks()}, nil
	case "skills":
		return Result{Output: c.skills()}, nil
	case "mcp":
		return Result{Output: c.mcp()}, nil
	case "settings":
		return c.settingsCmd(args)
	case "debug-log":
		return c.debugLogCmd(args)
	case "memory":
		return c.memoryCmd(ctx, args)
	case "web":
		return c.web(args)
	case "exit", "quit", "q":
		return Result{Exit: true}, nil
	default:
		return Result{}, fmt.Errorf("unknown command /%s (try /help)", name)
	}
}

func (c *Console) helpText() string {
	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, cmd := range Commands() {
		fmt.Fprintf(&b, "  /%-13s %s\n", cmd.Name, cmd.Description)
	}
	return b.String()
}

func (c *Console) status() string {
	d := c.deps
	var b strings.Builder
	fmt.Fprintf(&b, "woca %s\n", d.Version)
	fmt.Fprintf(&b, "provider=%s model=%s\n", d.Cfg.Provider, d.Cfg.Model)
	fmt.Fprintf(&b, "permission mode=%s plan mode=%v\n", d.Agent.Mode(), d.Agent.PlanMode())
	if s := d.Agent.TaskSummary(); s != "" {
		fmt.Fprintf(&b, "working on: %s\n", s)
	}
	fmt.Fprintf(&b, "workdir=%s\n", d.Cfg.WorkDir)
	if d.Web != nil {
		if addr, ok := d.Web.Running(); ok {
			fmt.Fprintf(&b, "web gui=http://%s\n", addr)
		} else {
			fmt.Fprintf(&b, "web gui=off (/web to start on port %d)\n", webgui.DefaultPort)
		}
	}
	return b.String()
}

func (c *Console) permissions(args []string) (Result, error) {
	if len(args) == 0 {
		return Result{Output: fmt.Sprintf("current mode=%s (use /permissions <mode>: default | acceptEdits | bypassPermissions | readOnly | plan)\n", c.deps.Agent.Mode())}, nil
	}
	mode := config.PermissionMode(args[0])
	switch mode {
	case config.ModeDefault, config.ModeAcceptEdits, config.ModeBypass, config.ModeReadOnly, config.ModePlan:
	default:
		return Result{}, fmt.Errorf("unknown permission mode %q", args[0])
	}
	c.deps.Agent.SetMode(mode)
	c.deps.Cfg.PermissionMode = mode
	return Result{Output: "permission mode set to " + string(mode) + "\n"}, nil
}

func (c *Console) model(args []string) (Result, error) {
	if len(args) == 0 {
		return Result{Output: "current model=" + c.deps.Cfg.Model + " (use /model <name> to switch)\n"}, nil
	}
	name := args[0]
	if c.deps.ModelFactory == nil {
		return Result{}, fmt.Errorf("model switching is not available in this session")
	}
	m, err := c.deps.ModelFactory(name)
	if err != nil {
		return Result{}, fmt.Errorf("build model %s: %w", name, err)
	}
	if err := c.deps.Agent.SetModel(m); err != nil {
		return Result{}, err
	}
	c.deps.Cfg.Model = name
	return Result{Output: "model switched to " + name + "\n"}, nil
}

func (c *Console) compact(ctx context.Context, session *Session) (Result, error) {
	before := tokens.Estimate(session.History)
	out := c.deps.Agent.ForceCompact(ctx, session.History)
	if len(out) == len(session.History) && tokens.Estimate(out) >= before {
		return Result{Output: "nothing to compact (already under the summary threshold)\n"}, nil
	}
	session.History = out
	return Result{Output: fmt.Sprintf("compacted: ~%d → ~%d tokens\n", before, tokens.Estimate(out))}, nil
}

func (c *Console) context(session *Session) string {
	total := tokens.Estimate(session.History)
	roles := map[messages.Role]int{}
	toolResults := 0
	for _, m := range session.History {
		roles[m.Role]++
		if m.Role == messages.RoleTool {
			toolResults++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "context usage: ~%d tokens across %d messages", total, len(session.History))
	if len(roles) > 0 {
		fmt.Fprintf(&b, " (human=%d ai=%d tool=%d)", roles[messages.RoleHuman], roles[messages.RoleAI], toolResults)
	}
	b.WriteString("\n")
	if limit := c.deps.Agent.CompactThreshold(); limit > 0 {
		pct := 0
		if limit > 0 {
			pct = total * 100 / limit
		}
		fmt.Fprintf(&b, "auto-compact threshold: %d tokens (now ~%d%%)\n", limit, pct)
	} else {
		b.WriteString("auto-compact: disabled (enable with --auto-compact <tokens>)\n")
	}
	// Session usage line (/cost lite): provider-reported totals survive
	// compaction, unlike the history estimate above.
	if u := c.deps.Agent.Usage(); u.Requests > 0 {
		fmt.Fprintf(&b, "session usage: %d requests · %d in / %d out tokens (see /cost)\n",
			u.Requests, u.InputTokens, u.OutputTokens)
	}
	return b.String()
}

func (c *Console) todos() string {
	todos := c.deps.Agent.Todos()
	if len(todos) == 0 {
		return "no todos (the model tracks work via TodoWrite)\n"
	}
	var b strings.Builder
	for _, t := range todos {
		mark := " "
		switch t.Status {
		case "in_progress":
			mark = ">"
		case "completed":
			mark = "x"
		case "cancelled":
			mark = "-"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, t.Content)
	}
	return b.String()
}

func (c *Console) tasks() string {
	tasks := c.deps.Agent.Tasks()
	if len(tasks) == 0 {
		return "no tasks (the model creates shared tasks via TaskCreate)\n"
	}
	var b strings.Builder
	for _, t := range tasks {
		fmt.Fprintf(&b, "  %-10s %-10s %s\n", t.ID, t.Status, t.Subject)
	}
	return b.String()
}

func (c *Console) skills() string {
	list := c.deps.Agent.Skills()
	if len(list) == 0 {
		return "no skills loaded (drop a SKILL.md under .woca/skills/<name>/)\n"
	}
	var b strings.Builder
	for _, s := range list {
		if s.Description != "" {
			fmt.Fprintf(&b, "  %-16s %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&b, "  %s\n", s.Name)
		}
	}
	return b.String()
}

func (c *Console) mcp() string {
	byServer := map[string][]string{}
	for _, name := range c.deps.Agent.ToolNames() {
		if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
			server, _, _ := strings.Cut(rest, "__")
			byServer[server] = append(byServer[server], name)
		}
	}
	if len(byServer) == 0 {
		return "no MCP servers connected (start one with --mcp name=command or name=https://endpoint)\n"
	}
	servers := make([]string, 0, len(byServer))
	for s := range byServer {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "%s (%d tools):\n", s, len(byServer[s]))
		for _, tool := range byServer[s] {
			fmt.Fprintf(&b, "  %s\n", tool)
		}
	}
	return b.String()
}

// web implements /web: no args (or an addr) starts the GUI, `off`/`stop` stops
// it, `status` reports it.
func (c *Console) web(args []string) (Result, error) {
	if c.deps.Web == nil {
		return Result{}, fmt.Errorf("web gui is not available in this session")
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	switch strings.ToLower(arg) {
	case "off", "stop":
		if _, running := c.deps.Web.Running(); !running {
			return Result{Output: "web gui is not running\n"}, nil
		}
		if err := c.deps.Web.Stop(); err != nil {
			return Result{}, err
		}
		return Result{Output: "web gui stopped\n"}, nil
	case "status":
		if addr, running := c.deps.Web.Running(); running {
			return Result{Output: fmt.Sprintf("web gui running at http://%s\n", addr)}, nil
		}
		return Result{Output: fmt.Sprintf("web gui off (default port %d)\n", webgui.DefaultPort)}, nil
	case "":
		return c.webStart("")
	default:
		// A port ("3484") or a full address ("127.0.0.1:3484").
		addr := arg
		if _, err := strconv.Atoi(arg); err == nil {
			addr = "127.0.0.1:" + arg
		}
		return c.webStart(addr)
	}
}

func (c *Console) webStart(addr string) (Result, error) {
	if cur, running := c.deps.Web.Running(); running {
		target := addr
		if target == "" {
			target = webgui.DefaultAddr
		}
		if cur == target {
			return Result{Output: fmt.Sprintf("web gui already running at http://%s (/web off to stop)\n", cur)}, nil
		}
		if err := c.deps.Web.Stop(); err != nil {
			return Result{}, err
		}
	}
	url, err := c.deps.Web.Start(addr)
	if err != nil {
		return Result{}, err
	}
	if c.deps.OpenBrowser != nil {
		c.deps.OpenBrowser(url)
	}
	return Result{Output: fmt.Sprintf("web gui started: %s (/web off to stop)\n", url)}, nil
}
