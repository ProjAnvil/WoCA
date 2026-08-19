// Command woca is the personal-edition entry point: a Go coding-agent
// harness with an explicit langgraph-style graph (model -> tools loop), a
// coding-agent tool set, hooks, permissions with human-in-the-loop
// approval, plan mode, subagents, skills, auto-compaction, and MCP — with a
// cobra CLI and zerolog logging.
//
// It bundles every frontend: the terminal UIs below plus the server roles
// imported from internal/servefront. Future server-only entries
// (woca-server, woca-gateway, ...) assemble from internal/agentbuild and
// internal/servefront without linking this package's terminal frontends.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/oauth"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/plugins"
	"github.com/projanvil/woca/internal/repl"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/sdkout"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tui"
	"github.com/projanvil/woca/internal/webgui"
	webdist "github.com/projanvil/woca/web"
)

const version = "0.3.0"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	opts := &agentbuild.Options{}
	root := &cobra.Command{
		Use:          "woca",
		Short:        "woca is a langgraph-style coding-agent harness",
		Version:      version,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), *opts, os.Stdin, os.Stdout, os.Stderr)
		},
	}
	// Persistent flags are inherited by subcommands (tui, serve, ...) so the
	// agent configuration can be set after the subcommand name as well.
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the woca version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("woca", version)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Run the interactive terminal UI (bubbletea)",
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(opts.LogLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			ap, err := agentbuild.BuildAgent(cmd.Context(), *opts, log)
			if err != nil {
				return err
			}
			defer ap.PluginMgr.Close()
			defer ap.Agent.Close()
			cons := newConsole(ap, log)
			runErr := tui.Run(ap.Agent, nil, ap.Cfg, version, tui.WithConsole(cons), tui.WithSettingsStore(ap.Settings))
			_ = ap.Agent.RunSessionEnd(cmd.Context(), "exit")
			return runErr
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Log in via OAuth 2.0 device flow",
		RunE: func(cmd *cobra.Command, args []string) error {
			return login(cmd.Context())
		},
	})

	root.AddCommand(servefront.NewServeCmd(opts))
	root.AddCommand(newConfigCmd())
	root.AddCommand(newPluginCmd())
	root.AddCommand(newMcpAuthCmd())
	root.AddCommand(servefront.NewWebCmd())
	return root
}

// newMcpAuthCmd builds `woca mcp auth <name> <https-endpoint>`: run the MCP
// OAuth flow for an HTTP-transport server and store the token encrypted.
func newMcpAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server utilities",
	}
	auth := &cobra.Command{
		Use:   "auth <name> <https-endpoint>",
		Short: "Authorize an MCP server via OAuth (stores the token encrypted)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dbTarget, _ := cmd.Flags().GetString("db")
			if dbTarget == "" {
				dbTarget = os.Getenv("WOCA_DB")
			}
			st, err := settings.Open(dbTarget)
			if err != nil {
				return err
			}
			if st == nil {
				return fmt.Errorf("settings database disabled (--db none)")
			}
			flow := mcp.OAuthFlow{
				ServerName: args[0],
				Endpoint:   args[1],
				Client:     &http.Client{Timeout: 15 * time.Second},
				OpenBrowser: func(u string) error {
					agentbuild.OpenBrowser(u)
					return nil
				},
				SaveToken: func(token string) error {
					return st.SetSecret(mcp.McpTokenSecretPrefix+args[0]+"_token", token)
				},
			}
			fmt.Printf("Authorizing MCP server %s (%s) — a browser window should open…\n", args[0], args[1])
			if err := mcp.RunOAuth(cmd.Context(), flow); err != nil {
				return err
			}
			fmt.Println("Authorized. Connect with --mcp", args[0]+"="+args[1])
			return nil
		},
	}
	cmd.AddCommand(auth)
	return cmd
}

// newPluginCmd builds `woca plugin list|install|remove` for managing the
// plugin root (~/.woca/plugins) outside the interactive UIs.
func newPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage plugins (skills and subagent bundles)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			list, errs := plugins.Load(plugins.DefaultRoot())
			for _, err := range errs {
				fmt.Fprintln(os.Stderr, "woca:", err)
			}
			if len(list) == 0 {
				fmt.Println("no plugins installed (woca plugin install <path-or-git-url>)")
				return nil
			}
			for _, p := range list {
				extra := ""
				if n := len(p.Skills) + len(p.Agents); n > 0 {
					extra = fmt.Sprintf(" (%d skills, %d agents)", len(p.Skills), len(p.Agents))
				}
				fmt.Printf("%-20s %-8s %s%s\n", p.Manifest.Name, p.Manifest.Version, p.Manifest.Description, extra)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "install <path-or-git-url>",
		Short: "Install a plugin from a local directory or git URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := plugins.Install(args[0], plugins.DefaultRoot())
			if err != nil {
				return err
			}
			fmt.Println("installed:", path)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := plugins.Uninstall(args[0], plugins.DefaultRoot()); err != nil {
				return err
			}
			fmt.Println("removed:", args[0])
			return nil
		},
	})
	return cmd
}

// newConfigCmd builds `woca config list|get|set` for managing persisted
// settings outside the interactive UIs.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View or update persisted settings (stored in the woca database)",
	}
	// The --db flag is parsed at Execute time (after this constructor ran),
	// so read it off the command instead of capturing an options copy.
	open := func(cmd *cobra.Command) (*settings.Store, error) {
		dbTarget, _ := cmd.Flags().GetString("db")
		if dbTarget == "" {
			dbTarget = os.Getenv("WOCA_DB")
		}
		st, err := settings.Open(dbTarget)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("settings database disabled (--db none)")
		}
		return st, nil
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show all settings (secrets masked)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			fmt.Print(settings.Describe(st.Load(), st.HasSecret, st.Path()))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Show one setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			key := args[0]
			if name, ok := settings.SecretFor(key); ok {
				if st.HasSecret(name) {
					fmt.Println("(set, encrypted)")
				} else {
					fmt.Println("(not set)")
				}
				return nil
			}
			cfg := st.Load()
			fmt.Println(settings.KeyValue(cfg, key))
			return nil
		},
	})
	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Update a setting (API keys are encrypted at rest)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			key, value := args[0], args[1]
			if name, ok := settings.SecretFor(key); ok {
				if err := st.SetSecret(name, value); err != nil {
					return err
				}
				fmt.Printf("%s saved (encrypted)\n", key)
				return nil
			}
			cfg := st.Load()
			if err := settings.Apply(&cfg, key, value); err != nil {
				return err
			}
			if err := st.Save(cfg); err != nil {
				return err
			}
			fmt.Printf("%s saved\n", key)
			return nil
		},
	}
	cmd.AddCommand(setCmd)
	return cmd
}

// login runs the OAuth device flow and saves the token.
func login(ctx context.Context) error {
	cfg := oauth.Config{
		ClientID:       os.Getenv("WOCA_OAUTH_CLIENT_ID"),
		DeviceEndpoint: os.Getenv("WOCA_OAUTH_DEVICE_ENDPOINT"),
		TokenEndpoint:  os.Getenv("WOCA_OAUTH_TOKEN_ENDPOINT"),
	}
	if cfg.ClientID == "" || cfg.DeviceEndpoint == "" || cfg.TokenEndpoint == "" {
		return fmt.Errorf("login requires WOCA_OAUTH_CLIENT_ID, WOCA_OAUTH_DEVICE_ENDPOINT, and WOCA_OAUTH_TOKEN_ENDPOINT")
	}
	dev, err := oauth.BeginDeviceFlow(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("Open %s and enter code: %s\n", dev.VerificationURI, dev.UserCode)
	tok, err := oauth.PollDeviceToken(ctx, cfg, dev, func(s string) {
		fmt.Fprintln(os.Stderr, s)
	})
	if err != nil {
		return err
	}
	dir := filepath.Join(session.DefaultDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600); err != nil {
		return err
	}
	fmt.Println("Logged in. Credentials saved to", filepath.Join(dir, "credentials.json"))
	return nil
}

func run(ctx context.Context, opts agentbuild.Options, stdin io.Reader, stdout, stderr io.Writer) error {
	level, err := logging.ParseLevel(opts.LogLevel)
	if err != nil {
		return err
	}
	log := logging.NewConsole(stderr, level)

	ap, err := agentbuild.BuildAgent(ctx, opts, log)
	if err != nil {
		return err
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	a, cfg := ap.Agent, ap.Cfg
	if opts.ResumeCheckpoint != "" {
		result, err := a.ResumeCheckpoint(ctx, opts.ResumeCheckpoint, opts.ResumeValue)
		if err != nil {
			return err
		}
		for i := len(result) - 1; i >= 0; i-- {
			if result[i].Role == messages.RoleAI && result[i].Content != "" {
				fmt.Fprintln(stdout, result[i].Content)
				break
			}
		}
		return nil
	}
	if fb, err := a.RunSessionStart(ctx); err == nil && fb != "" {
		log.Info().Str("feedback", fb).Msg("session-start hook feedback")
	}

	in := bufio.NewReader(stdin)
	interactive := opts.Prompt == ""
	handle := interruptHandler(in, interactive, stderr)

	// SDK output (--output-format json | stream-json with --prompt): machine
	// readable output instead of the human renderer.
	var sdk *sdkout.Writer
	if opts.Prompt != "" && opts.OutputFormat != "" {
		w, err := sdkout.New(opts.OutputFormat, stdout)
		if err != nil {
			return err
		}
		sdk = w
	}

	var render runner.Renderer = runner.DefaultRenderer(stdout)
	if sdk != nil {
		render = sdk.Step
	}
	r := runner.New(a, handle, render)
	r.RenderSummary = runner.DefaultSummaryRenderer(stdout)
	if sdk != nil {
		r.RenderSummary = nil
	}
	r.TokenBudget = turnTokenBudgetFromEnv()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load resume history if requested.
	var history []messages.Message
	if opts.Resume != "" {
		path := opts.Resume
		if path == "last" {
			path = filepath.Join(session.DefaultDir(), "last-session.jsonl")
		}
		st, err := session.LoadState(path)
		if err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		if len(st.Messages) == 0 {
			// Fall back to the legacy JSONL transcript format.
			loaded, err := session.Load(path)
			if err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			st.Messages = loaded
		}
		history = stripSystem(st.Messages)
		a.SetPlanMode(st.PlanMode)
		a.SetTodos(todosFromSession(st.Todos))
		a.SetTasks(tasksFromSession(st.Tasks))
	}

	if opts.Prompt != "" {
		history = append(history, messages.Human(submitHooks(ctx, a, opts.Prompt)))
		result, err := r.Run(ctx, history)
		if sdk != nil {
			turns := 1
			for _, m := range stripSystem(result) {
				if m.Role == messages.RoleTool {
					turns++
				}
			}
			sdk.Final(result, a.Usage(), cfg.Model, turns, err)
		}
		if endErr := a.RunSessionEnd(ctx, "oneshot"); endErr != nil && err == nil {
			err = endErr
		}
		return err
	}

	// Interactive REPL with a slash-command registry.
	cons := newConsole(ap, log)

	// Interactive: launch the TUI on a terminal, else fall back to the plain
	// line-based REPL (piped input / tests).
	if isTerminal(stdin) {
		err := tui.Run(a, history, cfg, version, tui.WithConsole(cons), tui.WithSettingsStore(ap.Settings))
		_ = a.RunSessionEnd(context.Background(), "exit")
		return err
	}
	loop := newREPLLoop(r, history, in, stdout, stderr, a, cons)
	return loop.Loop(ctx)
}

// newConsole builds the shared slash-command layer used by both the TUI and
// the line REPL, including the on-demand web GUI (/web) and settings (/settings).
func newConsole(ap *agentbuild.App, log zerolog.Logger) *console.Console {
	a, cfg := ap.Agent, ap.Cfg
	sessionDir := ""
	if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	web := webgui.New(server.Config{
		Agent:        a,
		Static:       webdist.DistFS(),
		Logger:       log,
		SessionDir:   sessionDir,
		WorkDir:      cfg.WorkDir,
		Settings:     ap.Settings,
		Memory:       ap.Memory,
		MemoryEngine: ap.MemEngine,
		RebuildMemory: func() error {
			return ap.RebuildMemory()
		},
		RebuildModel: func() error {
			return ap.RebuildModel()
		},
		CfgLive:           func() config.Config { return ap.CurrentCfg() },
		SetThinkingBudget: ap.SetThinkingBudget,
		Quota:             ap.FetchQuota,
		ApplyClaudeEnv:    ap.ApplyClaudeEnv,
		Plugins: func(enabled []string) []pluginrt.Status {
			return ap.PluginMgr.Statuses(enabled)
		},
		InstallPlugin: func(ctx context.Context, name string, reinstall bool) error {
			if reinstall {
				return ap.PluginMgr.Reinstall(ctx, name)
			}
			return ap.PluginMgr.Install(ctx, name)
		},
	})
	factory := func(name string) (language.ChatModel, error) {
		c := cfg
		// Re-resolve credentials so a provider/baseURL/key change made via
		// /settings applies to subsequent /model switches in the same session.
		dbSettings := ap.Settings.Load()
		provider := string(c.Provider)
		if ap.Opts.Provider == "" {
			provider = dbSettings.Provider
		}
		c.Provider = config.Provider(provider)
		c.APIKey = agentbuild.ResolveKey(ap.Opts.APIKey, provider, ap.Settings)
		c.BaseURL = agentbuild.ResolveBaseURL(ap.Opts.BaseURL, provider, dbSettings)
		c.Model = name
		return config.BuildModel(c)
	}
	return console.New(console.Deps{
		Agent:        a,
		Cfg:          &cfg,
		Web:          web,
		ModelFactory: factory,
		Version:      version,
		OpenBrowser:  agentbuild.OpenBrowser,
		Settings:     ap.Settings,
		Memory:       ap.Memory,
		MemoryEngine: ap.MemEngine,
		MemoryWorkspace: func() string {
			return a.MemoryWorkspace()
		},
		RebuildMemory: func() error {
			return ap.RebuildMemory()
		},
		RebuildModel: func() error {
			return ap.RebuildModel()
		},
		CfgLive:           func() config.Config { return ap.CurrentCfg() },
		SetThinkingBudget: ap.SetThinkingBudget,
		Quota:             ap.FetchQuota,
		ApplyTheme:        tui.SetTheme,
		SetVimModeToggle: func() bool {
			tui.SetVimMode(!tui.VimMode())
			return tui.VimMode()
		},
	})
}

// submitHooks runs UserPromptSubmit hooks and prepends their feedback.
func submitHooks(ctx context.Context, a *agent.Agent, prompt string) string {
	if fb, err := a.RunUserPromptSubmit(ctx, prompt); err == nil && fb != "" {
		return fb + "\n\n" + prompt
	}
	return prompt
}

// isTerminal reports whether r is an interactive terminal (has an Fd and is a
// TTY).
func isTerminal(r io.Reader) bool {
	f, ok := r.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// replLoop is the interactive read-eval loop.
type replLoop struct {
	r       *runner.Runner
	history []messages.Message
	editor  *repl.Editor
	stdout  io.Writer
	stderr  io.Writer
	cons    *console.Console
	agent   *agent.Agent
	hist    *repl.History
}

func newREPLLoop(r *runner.Runner, history []messages.Message, stdin io.Reader, stdout, stderr io.Writer, a *agent.Agent, cons *console.Console) *replLoop {
	hist := &repl.History{}
	return &replLoop{r: r, history: history, editor: repl.NewEditor(stdin, stdout, hist), stdout: stdout, stderr: stderr, cons: cons, agent: a, hist: hist}
}

func (r *replLoop) Loop(ctx context.Context) error {
	for {
		line, err := r.editor.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}
			continue // Ctrl-C: reprompt
		}
		action := repl.Parse(line)
		switch action.Kind {
		case repl.KindQuit:
			endSession(ctx, r.history, r.agent)
			return nil
		case repl.KindCommand:
			session := &console.Session{History: r.history}
			res, err := r.cons.Execute(ctx, action.Text, action.Args, session)
			if err != nil {
				fmt.Fprintln(r.stderr, "woca:", err)
				continue
			}
			r.history = session.History
			if res.Output != "" {
				fmt.Fprint(r.stdout, res.Output)
			}
			if res.Exit {
				endSession(ctx, r.history, r.agent)
				return nil
			}
			if res.Prompt != "" {
				// /init-style commands hand back a prompt to submit as a full
				// user turn.
				r.submit(ctx, res.Prompt)
			}
		case repl.KindShell:
			out := runShellCommand(action.Text)
			fmt.Fprint(r.stdout, out)
			r.history = append(r.history, messages.Human("[shell]\n$ "+action.Text+"\n"+out))
		default: // KindPrompt
			r.submit(ctx, submitHooks(ctx, r.agent, action.Text))
		}
	}
	endSession(ctx, r.history, r.agent)
	return nil
}

// submit appends a user message and runs one agent turn, keeping the history
// in sync on both success and failure.
func (r *replLoop) submit(ctx context.Context, text string) {
	r.history = append(r.history, messages.Human(text))
	result, err := r.r.Run(ctx, r.history)
	if err != nil {
		fmt.Fprintln(r.stderr, "woca:", err)
		r.history = r.history[:len(r.history)-1]
		return
	}
	r.history = stripSystem(result)
}

// runShellCommand executes a `!command` line and returns its combined output.
// It stays on plain os/exec: the shell escape is a terminal-frontend
// convenience that always runs on the machine woca was started from.
func runShellCommand(command string) string {
	cmd := exec.Command("bash", "-c", command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(&out, "\n[exit: %v]", err)
	}
	return out.String()
}

// turnTokenBudgetFromEnv resolves WOCA_TURN_TOKEN_BUDGET: the per-turn
// output-token budget for auto-continue (e.g. a 500k budget).
// Unset, empty, or non-positive disables it — normal turns end naturally.
func turnTokenBudgetFromEnv() int {
	v := strings.TrimSpace(os.Getenv("WOCA_TURN_TOKEN_BUDGET"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// interruptHandler resolves graph interrupts in interactive mode and
// auto-denies in one-shot mode.
func interruptHandler(stdin *bufio.Reader, interactive bool, stderr io.Writer) runner.InterruptHandler {
	return func(value any) (any, error) {
		switch v := value.(type) {
		case agent.PermissionRequest:
			fmt.Fprintf(stderr, "\nPermission requested: %s — %s\n", v.ToolName, v.Reason)
			if !interactive {
				fmt.Fprintln(stderr, "  (non-interactive: denied; use --dangerously-skip-permissions or --accept-edits to allow)")
				return "n", nil
			}
			fmt.Fprint(stderr, "  Allow? [y/N] ")
			return readYesNo(stdin)

		case agent.AskUserRequest:
			for i, q := range v.Questions {
				fmt.Fprintf(stderr, "\n%s", tools.FormatQuestion(i, q))
			}
			if !interactive {
				return agent.AskUserAnswer{}, nil
			}
			answers := make([]string, 0, len(v.Questions))
			for i, q := range v.Questions {
				fmt.Fprintf(stderr, "  Answer %d%s: ", i+1, multiHint(q.MultiSelect))
				line, err := stdin.ReadString('\n')
				if err != nil {
					answers = append(answers, "")
					continue
				}
				answers = append(answers, strings.TrimSpace(line))
			}
			return agent.AskUserAnswer{Answers: answers}, nil

		case agent.PlanApprovalRequest:
			fmt.Fprintln(stderr, "\nPlan proposed for approval:")
			if v.Plan != "" {
				fmt.Fprintln(stderr, v.Plan)
			}
			if !interactive {
				return "n", nil
			}
			fmt.Fprint(stderr, "  Approve plan? [y/N] ")
			return readYesNo(stdin)

		default:
			return nil, fmt.Errorf("unknown interrupt value %T", value)
		}
	}
}

func multiHint(multi bool) string {
	if multi {
		return " (comma-separated)"
	}
	return ""
}

func readYesNo(stdin *bufio.Reader) (any, error) {
	line, err := stdin.ReadString('\n')
	if err != nil {
		return "n", nil
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		line = "n"
	}
	return line, nil
}

// endSession persists the last-session snapshot and runs SessionEnd hooks.
func endSession(ctx context.Context, history []messages.Message, a *agent.Agent) {
	saveLastSession(history, a)
	if a != nil {
		_ = a.RunSessionEnd(ctx, "exit")
	}
}

func saveLastSession(history []messages.Message, a *agent.Agent) {
	if len(history) == 0 && a == nil {
		return
	}
	path := filepath.Join(session.DefaultDir(), "last-session.jsonl")
	st := session.State{Messages: history}
	if a != nil {
		st.PlanMode = a.PlanMode()
		st.Todos = todosToSession(a.Todos())
		st.Tasks = tasksToSession(a.Tasks())
	}
	if err := session.SaveState(path, st); err != nil {
		fmt.Fprintf(os.Stderr, "woca: save session: %v\n", err)
	}
}

// todosToSession maps agent todos to the persisted session representation.
func todosToSession(todos []tools.Todo) []session.Todo {
	out := make([]session.Todo, 0, len(todos))
	for _, t := range todos {
		out = append(out, session.Todo{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm})
	}
	return out
}

// todosFromSession maps persisted todos back to agent todos.
func todosFromSession(todos []session.Todo) []tools.Todo {
	out := make([]tools.Todo, 0, len(todos))
	for _, t := range todos {
		out = append(out, tools.Todo{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm})
	}
	return out
}

// tasksToSession maps agent tasks to the persisted session representation.
func tasksToSession(tasks []agent.TaskRecord) []session.Task {
	out := make([]session.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, session.Task{ID: t.ID, Subject: t.Subject, Description: t.Description, Status: string(t.Status)})
	}
	return out
}

// tasksFromSession maps persisted tasks back to agent tasks.
func tasksFromSession(tasks []session.Task) []agent.TaskRecord {
	out := make([]agent.TaskRecord, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, agent.TaskRecord{ID: t.ID, Subject: t.Subject, Description: t.Description, Status: agent.TaskRecordStatus(t.Status)})
	}
	return out
}

func stripSystem(msgs []messages.Message) []messages.Message {
	out := msgs[:0]
	for _, m := range msgs {
		if m.Role != messages.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}
