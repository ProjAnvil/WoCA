// Command woca is a Go coding-agent harness modeled on the architecture of
// Claude Code: an explicit langgraph-style graph (model -> tools loop), a
// Claude-Code-style tool set, hooks, permissions with human-in-the-loop
// approval, plan mode, subagents, skills, auto-compaction, and MCP — with a
// cobra CLI and zerolog logging.
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	contextx "github.com/projanvil/woca/internal/context"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/oauth"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/plugins"
	"github.com/projanvil/woca/internal/prompt"
	"github.com/projanvil/woca/internal/repl"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/sdkout"
	"github.com/projanvil/woca/internal/searchapi"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tui"
	"github.com/projanvil/woca/internal/webd"
	"github.com/projanvil/woca/internal/webgui"
	"github.com/projanvil/woca/internal/wocadb"
)

const version = "0.3.0"

type options struct {
	prompt           string
	model            string
	provider         string
	apiKey           string
	baseURL          string
	maxTokens        int
	thinkingBudget   int
	outputFormat     string
	managedSettings  string
	temperature      float64
	mode             string
	skipPerms        bool
	acceptEdits      bool
	readOnly         bool
	plan             bool
	workdir          string
	extraPrompt      string
	resume           string
	logLevel         string
	compactLimit     int
	mcpServers       []string
	fallbackModel    string
	searchAPI        string
	searchAPIKey     string
	checkpointDir    string
	resumeCheckpoint string
	resumeValue      string
	allowCommands    []string
	denyCommands     []string
	db               string
	maxTurns         int
	debugLog         bool
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	opts := &options{}
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
	fl := root.PersistentFlags()
	fl.StringVarP(&opts.prompt, "prompt", "p", "", "one-shot prompt; runs non-interactively and exits")
	fl.StringVar(&opts.model, "model", "", "model name (e.g. claude-sonnet-4-5, gpt-4o)")
	fl.StringVar(&opts.provider, "provider", "", "provider: anthropic or openai")
	fl.StringVar(&opts.apiKey, "api-key", "", "API key (falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY)")
	fl.StringVar(&opts.baseURL, "base-url", "", "provider base URL")
	fl.IntVar(&opts.maxTokens, "max-tokens", 0, "maximum output tokens")
	fl.IntVar(&opts.thinkingBudget, "thinking-budget", 0, "extended-thinking budget in tokens (0 = off; anthropic provider only, minimum 1024)")
	fl.StringVar(&opts.outputFormat, "output-format", "", "headless output format for --prompt runs: json or stream-json (default plain text)")
	fl.StringVar(&opts.managedSettings, "managed-settings", "", "enterprise managed settings file (overrides and locks settings; env WOCA_MANAGED_SETTINGS)")
	fl.Float64Var(&opts.temperature, "temperature", 0, "sampling temperature")
	fl.StringVar(&opts.mode, "permission-mode", "", "default | acceptEdits | bypassPermissions | readOnly | plan")
	fl.BoolVar(&opts.skipPerms, "dangerously-skip-permissions", false, "allow all tools without prompting")
	fl.BoolVar(&opts.acceptEdits, "accept-edits", false, "auto-accept file edits; still prompt before shell commands")
	fl.BoolVar(&opts.readOnly, "read-only", false, "deny write/shell tools")
	fl.BoolVar(&opts.plan, "plan", false, "read-only planning mode")
	fl.StringVar(&opts.workdir, "workdir", "", "working directory (default: current directory)")
	fl.StringVar(&opts.extraPrompt, "system-prompt", "", "extra instructions appended to the system prompt")
	fl.StringVar(&opts.resume, "resume", "", "resume from a session transcript file (or \"last\")")
	fl.StringVar(&opts.logLevel, "log-level", "warn", "log level: trace | debug | info | warn | error | disabled")
	fl.IntVar(&opts.compactLimit, "auto-compact", 0, "auto-compact the conversation above this token estimate (0 disables)")
	fl.StringArrayVar(&opts.mcpServers, "mcp", nil, "MCP server as name=command or name=http(s)://endpoint (repeatable)")
	fl.StringVar(&opts.fallbackModel, "fallback-model", "", "fallback model name used after transient retries fail")
	fl.StringVar(&opts.searchAPI, "search-api", "", "JSON web-search API endpoint for WebSearch (with {q} placeholder)")
	fl.StringVar(&opts.searchAPIKey, "search-api-key", "", "bearer key for --search-api")
	fl.StringVar(&opts.checkpointDir, "checkpoint-dir", "", "directory to persist graph checkpoints (enables cross-process interrupt/resume)")
	fl.StringVar(&opts.resumeCheckpoint, "resume-checkpoint", "", "resume from a persisted checkpoint key")
	fl.StringVar(&opts.resumeValue, "resume-value", "", "resume value for --resume-checkpoint (e.g. y/n)")
	fl.StringArrayVar(&opts.allowCommands, "allow-command", nil, "Bash command prefix to allow (repeatable); when set, only these may run")
	fl.StringArrayVar(&opts.denyCommands, "deny-command", nil, "Bash command substring to block (repeatable)")
	fl.IntVar(&opts.maxTurns, "max-turns", 0, "cap the model rounds per turn (0 = unlimited)")
	fl.StringVar(&opts.db, "db", "", "settings database: sqlite path, postgres:// DSN, or \"none\" (default ~/.woca/woca.db; env WOCA_DB)")
	fl.BoolVar(&opts.debugLog, "debug-log", false, "enable the harness observability log (graph/state/session/message traces to ~/.woca/logs; also settable via settings debug-log)")

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
			level, err := logging.ParseLevel(opts.logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			ap, err := buildAgent(cmd.Context(), *opts, log)
			if err != nil {
				return err
			}
			cons := newConsole(ap, log)
			runErr := tui.Run(ap.a, nil, ap.cfg, version, tui.WithConsole(cons), tui.WithSettingsStore(ap.settings))
			_ = ap.a.RunSessionEnd(cmd.Context(), "exit")
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

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the embedded web UI server (frontend + SSE API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			noBrowser, _ := cmd.Flags().GetBool("no-browser")
			return serve(cmd.Context(), *opts, addr, noBrowser)
		},
	}
	serveCmd.Flags().String("addr", webgui.DefaultAddr, "listen address for the web server")
	serveCmd.Flags().Bool("no-browser", false, "do not open the browser on startup")
	root.AddCommand(serveCmd)

	root.AddCommand(newConfigCmd(*opts))
	root.AddCommand(newPluginCmd())
	root.AddCommand(newMcpAuthCmd(*opts))
	root.AddCommand(newWebCmd())
	return root
}

// newWebCmd builds `woca web start|stop|restart|status`: manage the web GUI
// as a background daemon (detached `woca serve`, pid/addr in ~/.woca/web.json,
// output in ~/.woca/web.log).
func newWebCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Manage the background web GUI (start | stop | restart | status)",
	}
	addrFlag := func(sub *cobra.Command) *string {
		return sub.Flags().String("addr", "", "listen address (default 127.0.0.1:3484; a bare port is accepted)")
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Start the web GUI in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := webd.New()
			addr, _ := cmd.Flags().GetString("addr")
			rec, started, err := m.Start(webd.ParseAddr(addr))
			if err != nil {
				return err
			}
			if started {
				fmt.Printf("web gui started in the background: http://%s (pid %d, log %s)\n", rec.Addr, rec.PID, m.LogPath())
			} else {
				fmt.Printf("web gui already running: http://%s (pid %d)\n", rec.Addr, rec.PID)
			}
			return nil
		},
	}
	addrFlag(start)
	cmd.AddCommand(start)

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the background web GUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := webd.New()
			stopped, err := m.Stop()
			if err != nil {
				return err
			}
			if stopped {
				fmt.Println("web gui stopped")
			} else {
				fmt.Println("web gui not running")
			}
			return nil
		},
	})

	restart := &cobra.Command{
		Use:   "restart",
		Short: "Restart the background web GUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := webd.New()
			addr, _ := cmd.Flags().GetString("addr")
			rec, err := m.Restart(webd.ParseAddr(addr))
			if err != nil {
				return err
			}
			fmt.Printf("web gui restarted: http://%s (pid %d)\n", rec.Addr, rec.PID)
			return nil
		},
	}
	addrFlag(restart)
	cmd.AddCommand(restart)

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether the background web GUI is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := webd.New()
			if rec := m.Status(); rec != nil {
				fmt.Printf("running: http://%s (pid %d, started %s)\n", rec.Addr, rec.PID, rec.Started)
				return nil
			}
			fmt.Println("not running (woca web start to launch)")
			return nil
		},
	})
	return cmd
}

// newMcpAuthCmd builds `woca mcp auth <name> <https-endpoint>`: run the MCP
// OAuth flow for an HTTP-transport server and store the token encrypted.
func newMcpAuthCmd(opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server utilities",
	}
	auth := &cobra.Command{
		Use:   "auth <name> <https-endpoint>",
		Short: "Authorize an MCP server via OAuth (stores the token encrypted)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dbTarget := opts.db
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
					if os.Getenv("WOCA_NO_BROWSER") == "" {
						_ = openBrowser(u)
					}
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
func newConfigCmd(opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View or update persisted settings (stored in the woca database)",
	}
	open := func() (*settings.Store, error) {
		target := opts.db
		if target == "" {
			target = os.Getenv("WOCA_DB")
		}
		st, err := settings.Open(target)
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
			st, err := open()
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
			st, err := open()
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
			st, err := open()
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

func run(ctx context.Context, opts options, stdin io.Reader, stdout, stderr io.Writer) error {
	level, err := logging.ParseLevel(opts.logLevel)
	if err != nil {
		return err
	}
	log := logging.NewConsole(stderr, level)

	ap, err := buildAgent(ctx, opts, log)
	if err != nil {
		return err
	}
	a, cfg := ap.a, ap.cfg
	if opts.resumeCheckpoint != "" {
		result, err := a.ResumeCheckpoint(ctx, opts.resumeCheckpoint, opts.resumeValue)
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
	interactive := opts.prompt == ""
	handle := interruptHandler(in, interactive, stderr)

	// SDK output (--output-format json | stream-json with --prompt): machine
	// readable output instead of the human renderer.
	var sdk *sdkout.Writer
	if opts.prompt != "" && opts.outputFormat != "" {
		w, err := sdkout.New(opts.outputFormat, stdout)
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
	if opts.resume != "" {
		path := opts.resume
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

	if opts.prompt != "" {
		history = append(history, messages.Human(submitHooks(ctx, a, opts.prompt)))
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
		err := tui.Run(a, history, cfg, version, tui.WithConsole(cons), tui.WithSettingsStore(ap.settings))
		_ = a.RunSessionEnd(context.Background(), "exit")
		return err
	}
	loop := newREPLLoop(r, history, in, stdout, stderr, a, cons)
	return loop.Loop(ctx)
}

// newConsole builds the shared slash-command layer used by both the TUI and
// the line REPL, including the on-demand web GUI (/web) and settings (/settings).
func newConsole(ap *app, log zerolog.Logger) *console.Console {
	a, cfg := ap.a, ap.cfg
	sessionDir := ""
	if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	web := webgui.New(server.Config{
		Agent:        a,
		Static:       woca.DistFS(),
		Logger:       log,
		SessionDir:   sessionDir,
		WorkDir:      cfg.WorkDir,
		Settings:     ap.settings,
		Memory:       ap.memory,
		MemoryEngine: ap.memEngine,
		RebuildMemory: func() error {
			return ap.rebuildMemory()
		},
		RebuildModel: func() error {
			return ap.rebuildModel()
		},
	})
	factory := func(name string) (language.ChatModel, error) {
		c := cfg
		// Re-resolve credentials so a provider/baseURL/key change made via
		// /settings applies to subsequent /model switches in the same session.
		dbSettings := ap.settings.Load()
		provider := string(c.Provider)
		if ap.opts.provider == "" {
			provider = dbSettings.Provider
		}
		c.Provider = config.Provider(provider)
		c.APIKey = resolveKey(ap.opts.apiKey, provider, ap.settings)
		c.BaseURL = resolveBaseURL(ap.opts.baseURL, provider, dbSettings)
		c.Model = name
		return config.BuildModel(c)
	}
	open := func(url string) {
		if os.Getenv("WOCA_NO_BROWSER") == "" {
			_ = openBrowser(url)
		}
	}
	return console.New(console.Deps{
		Agent:        a,
		Cfg:          &cfg,
		Web:          web,
		ModelFactory: factory,
		Version:      version,
		OpenBrowser:  open,
		Settings:     ap.settings,
		Memory:       ap.memory,
		RebuildModel: func() error {
			return ap.rebuildModel()
		},
		ApplyTheme: tui.SetTheme,
		SetVimModeToggle: func() bool {
			tui.SetVimMode(!tui.VimMode())
			return tui.VimMode()
		},
	})
}

// serve runs the embedded web UI server.
func serve(ctx context.Context, opts options, addr string, noBrowser bool) error {
	level, err := logging.ParseLevel(opts.logLevel)
	if err != nil {
		return err
	}
	log := logging.NewConsole(os.Stderr, level)

	ap, err := buildAgent(ctx, opts, log)
	if err != nil {
		return err
	}

	sessionDir := ""
	if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	srv := server.New(server.Config{
		Agent:        ap.a,
		Static:       woca.DistFS(),
		Logger:       log,
		SessionDir:   sessionDir,
		WorkDir:      ap.cfg.WorkDir,
		Settings:     ap.settings,
		Memory:       ap.memory,
		MemoryEngine: ap.memEngine,
		RebuildMemory: func() error {
			return ap.rebuildMemory()
		},
		RebuildModel: func() error {
			return ap.rebuildModel()
		},
	})

	url := "http://" + addr
	if !noBrowser {
		_ = openBrowser(url)
	}
	fmt.Fprintf(os.Stderr, "woca web: %s\n", url)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx, addr)
}

// openBrowser opens url in the default browser (best-effort).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform %q", runtime.GOOS)
	}
	return cmd.Start()
}

// app bundles the assembled runtime: the agent, its effective config, the
// startup flags (for re-resolution), the persistent settings store, the
// memoryStore, and the factual-memory engine (ladyM; nil when disabled or
// unavailable). memMu guards the memEngine swap in rebuildMemory; the
// agent's own engine pointer carries its own lock.
type app struct {
	a         *agent.Agent
	cfg       config.Config
	opts      options
	settings  *settings.Store
	memory    *store.Store
	memMu     sync.Mutex
	memEngine memory.Engine
}

// buildAgent constructs the configured agent (model, prompt, hooks, skills,
// MCP tools, compaction, storage backends) shared by the REPL/one-shot, the
// TUI, and the web server. Provider credentials resolve in order: CLI flag →
// environment variable → settings database (API keys encrypted at rest) →
// OAuth token.
func buildAgent(ctx context.Context, opts options, log zerolog.Logger) (*app, error) {
	workdir := opts.workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	workdir, _ = filepath.Abs(workdir)

	mode := resolveMode(opts.mode, opts.skipPerms, opts.acceptEdits, opts.readOnly, opts.plan)

	dbTarget := opts.db
	if dbTarget == "" {
		dbTarget = os.Getenv("WOCA_DB")
	}
	st, err := settings.Open(dbTarget)
	if err != nil {
		log.Warn().Err(err).Msg("settings database unavailable; continuing without persisted settings")
		st = nil
	}
	// Enterprise managed settings: override and lock (highest precedence).
	if managed := managedSettingsPath(opts.managedSettings); managed != "" {
		if m, err := settings.LoadManaged(managed); err != nil {
			log.Warn().Err(err).Msg("managed settings file unreadable; ignoring")
		} else {
			st.SetManaged(m)
			log.Info().Str("path", managed).Msg("managed settings applied")
		}
	}
	dbSettings := st.Load()

	provider := opts.provider
	if provider == "" {
		provider = dbSettings.Provider
	}
	modelName := opts.model
	if modelName == "" {
		modelName = dbSettings.Model
	}
	maxTokens := opts.maxTokens
	if maxTokens == 0 {
		maxTokens = dbSettings.MaxTokens
	}
	temperature := opts.temperature
	if temperature == 0 {
		temperature = dbSettings.Temperature
	}

	cfg := config.Config{
		Provider:          config.Provider(provider),
		Model:             modelName,
		APIKey:            resolveKey(opts.apiKey, provider, st),
		BaseURL:           resolveBaseURL(opts.baseURL, provider, dbSettings),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		PermissionMode:    mode,
		WorkDir:           workdir,
		ExtraSystemPrompt: opts.extraPrompt,
		ThinkingBudget:    opts.thinkingBudget,
	}
	cfg, err = cfg.Normalize()
	if err != nil {
		return nil, err
	}

	model, err := config.BuildModel(cfg)
	if err != nil {
		return nil, err
	}

	// Optional fallback model for the retry/fallback router.
	var fallbackModel language.ChatModel
	if opts.fallbackModel != "" {
		fbCfg := cfg
		fbCfg.Model = opts.fallbackModel
		fbModel, err := config.BuildModel(fbCfg)
		if err != nil {
			return nil, err
		}
		fallbackModel = fbModel
	}

	// Assemble the system prompt with collected context.
	sysPrompt := prompt.BuildWithStyle(cfg.WorkDir, cfg.ExtraSystemPrompt, cfg.Model, dbSettings.OutputStyle)
	if c := contextx.Collect(cfg.WorkDir); c != "" {
		sysPrompt += "\n\n" + c
	}

	hookRunner := hooks.NewRunner(loadHooksConfig(workdir), workdir, "woca", hooks.WithLogger(log))
	skillList := loadSkills(workdir)
	agentDefs := loadAgents(workdir)
	// Plugins (marketplace-level units under ~/.woca/plugins) contribute
	// namespaced skills and subagent definitions.
	pluginList, pluginErrs := plugins.Load(plugins.DefaultRoot())
	for _, perr := range pluginErrs {
		log.Warn().Err(perr).Msg("plugin skipped")
	}
	for _, pl := range pluginList {
		skillList = append(skillList, pl.Skills...)
		agentDefs = append(agentDefs, pl.Agents...)
	}
	extraTools, mcpClients := connectMCP(ctx, log, opts.mcpServers, st)

	reg := tools.NewRegistry(cfg.WorkDir)
	if opts.searchAPI != "" {
		sc := searchapi.New(searchapi.Config{Endpoint: opts.searchAPI, APIKey: opts.searchAPIKey})
		reg.Register(tools.WebSearchToolWithBackend(func(ctx context.Context, q string) (string, error) {
			results, err := sc.Search(ctx, q)
			if err != nil {
				return "", err
			}
			return searchapi.Format(results), nil
		}))
	}

	// Storage backends: checkpoint persistence and the memoryStore, selected
	// from settings (sqlite under ~/.woca is the default). The --checkpoint-dir
	// flag keeps precedence for the file backend.
	var pgDB *wocadb.DB
	openPostgres := func() *wocadb.DB {
		if pgDB != nil {
			return pgDB
		}
		if st == nil {
			return nil
		}
		dsn := st.GetSecret(settings.SecretPostgresURL)
		if dsn == "" {
			log.Warn().Msg("postgres backend selected but no postgres_url secret is set")
			return nil
		}
		db, err := wocadb.Open("pg:" + dsn)
		if err != nil {
			log.Warn().Err(err).Msg("postgres backend unavailable")
			return nil
		}
		pgDB = db
		return pgDB
	}
	localDB := func() *wocadb.DB {
		if st == nil {
			return nil
		}
		return st.DB()
	}

	var checkpointer graph.Checkpointer
	backend := dbSettings.CheckpointBackend
	if backend == "" {
		backend = settings.CheckpointSQLite
	}
	switch {
	case opts.checkpointDir != "":
		checkpointer = graph.NewFileCheckpointer(opts.checkpointDir)
	case backend == settings.CheckpointFile:
		if dir := dbSettings.CheckpointDir; dir != "" {
			checkpointer = graph.NewFileCheckpointer(dir)
		}
	case backend == settings.CheckpointPostgres:
		if db := openPostgres(); db != nil {
			checkpointer = graph.NewSQLCheckpointer(db.DB, string(db.Dialect))
		}
	case backend == settings.CheckpointSQLite:
		if db := localDB(); db != nil {
			checkpointer = graph.NewSQLCheckpointer(db.DB, string(db.Dialect))
		}
	}

	var memory *store.Store
	switch dbSettings.MemoryBackend {
	case settings.MemoryInMem:
		memory = store.NewMemory()
	case settings.MemoryPostgres:
		if db := openPostgres(); db != nil {
			memory = store.NewSQL(db.DB, string(db.Dialect), db.Path)
		}
	case settings.MemorySQLite:
		if db := localDB(); db != nil {
			memory = store.NewSQL(db.DB, string(db.Dialect), db.Path)
		}
	}

	// Factual-memory engine (ladyM): startup build degrades to nil on
	// failure so the harness always comes up; settings changes rebuild it
	// live via app.rebuildMemory.
	memEngine, memErr := buildMemoryEngine(&app{cfg: cfg, settings: st}, dbSettings, model)
	if memErr != nil {
		log.Warn().Err(memErr).Msg("memory engine unavailable; continuing without memory")
		memEngine = nil
	}

	// Max-output-tokens escalation for the recovery path (Claude Code's
	// 8k→64k retry): wire the escalator only when the user did not set an
	// explicit cap, mirroring Claude Code's default-cap condition. BuildModel
	// keeps the thinking budget enabled and the cap above it when linked.
	var escalator func() (language.ChatModel, error)
	if opts.maxTokens == 0 && dbSettings.MaxTokens == 0 {
		escCfg := cfg
		escCfg.MaxTokens = agent.EscalatedMaxTokens
		escalator = func() (language.ChatModel, error) {
			return config.BuildModel(escCfg)
		}
	}

	// Observability log (default off): the flag forces it on; otherwise the
	// persisted debug-log setting decides. The store itself gates writes, so
	// it can be toggled live from the web settings UI or /debug-log.
	obsRoot := ""
	if home, err := os.UserHomeDir(); err == nil {
		obsRoot = filepath.Join(home, ".woca", "logs")
	}
	obs := obslog.New(obsRoot, opts.debugLog || dbSettings.DebugLog)
	if opts.debugLog {
		// The flag forces the log on for the whole process: dynamic refreshes
		// from the settings database cannot turn it back off.
		obs.LockOpen()
	}

	a, err := agent.New(agent.Config{
		Model:              model,
		FallbackModel:      fallbackModel,
		ModelMaxRetries:    2,
		Registry:           reg,
		SystemPrompt:       sysPrompt,
		PermissionMode:     cfg.PermissionMode,
		RecursionLimit:     cfg.RecursionLimit,
		WorkDir:            cfg.WorkDir,
		Hooks:              hookRunner,
		Skills:             skillList,
		ExtraTools:         extraTools,
		CompactThreshold:   opts.compactLimit,
		Summarizer:         model,
		Permissions:        loadPermissions(workdir),
		Agents:             agentDefs,
		CheckpointDir:      opts.checkpointDir,
		Checkpointer:       checkpointer,
		BashPolicy:         tools.NewCommandPolicy(opts.allowCommands, opts.denyCommands),
		MaxTokensEscalator: escalator,
		// Tool-use summaries are a UI feature: emit them for interactive
		// sessions (REPL/TUI/serve), not for one-shot --prompt runs —
		// Claude Code's isNonInteractiveSession gate.
		EmitToolUseSummaries: opts.prompt == "",
		// Between-turn tool refresh (stage 28): re-list MCP tools so
		// hot-added ones are usable in the next round.
		RefreshTools: func(ctx context.Context) []coretools.Tool {
			return refreshMCPTools(ctx, log, mcpClients)
		},
		// Long-term memory attachments: prefetch relevant entries per turn.
		Memory:       memory,
		MemoryEngine: memEngine,
		// Live settings for the memory-engine paths (top-k, autoretain…);
		// Store.Load is nil-safe so a failed settings open keeps working.
		LoadSettings: st.Load,
		MaxTurns:     opts.maxTurns,
		Obs:          obs,
	})
	if err != nil {
		return nil, err
	}
	a.SetMemoryWorkspace(cfg.WorkDir)
	return &app{a: a, cfg: cfg, opts: opts, settings: st, memory: memory, memEngine: memEngine}, nil
}

// buildMemoryEngine constructs the memory engine from current settings.
// engine=none returns (nil, nil); build failures degrade to nil (logged by
// the caller) so woca always starts.
func buildMemoryEngine(ap *app, dbSettings settings.Settings, chatModel language.ChatModel) (memory.Engine, error) {
	if dbSettings.MemoryEngine == settings.MemoryEngineNone {
		return nil, nil
	}
	dbPath := memory.ExpandHome(dbSettings.LadyMDBPath)
	if dbPath == "" {
		dbPath = memory.DefaultDBPath()
	}
	in := memory.BuildInput{
		EngineEnabled:    true,
		DBPath:           dbPath,
		DefaultWorkspace: ap.cfg.WorkDir,
		Embedding:        dbSettings.LadyMEmbedding,
		EmbeddingModel:   dbSettings.LadyMEmbeddingModel,
		EmbeddingBaseURL: dbSettings.LadyMEmbeddingBaseURL,
		EmbeddingAPIKey:  ap.settings.GetSecret(settings.SecretLadyMEmbeddingAPIKey),
		LLM:              dbSettings.LadyMLLM,
		ChatModel:        chatModel,
		OpenAIKey:        ap.settings.GetSecret(settings.SecretOpenAIAPIKey),
		OpenAIBaseURL:    dbSettings.OpenAIBaseURL,
	}
	eng, err := memory.Build(in)
	if err != nil {
		return nil, err
	}
	return eng, nil
}

// rebuildModel reconstructs the chat model from the current effective
// configuration (startup flags + live settings from the database) and swaps
// it into the agent. Used by the settings UIs after provider, base URL, or
// API key changes. The model name itself is not re-resolved here; callers
// that change it switch via SetModel directly.
func (ap *app) rebuildModel() error {
	dbSettings := ap.settings.Load()
	provider := ap.opts.provider
	if provider == "" {
		provider = dbSettings.Provider
	}
	cfg := ap.cfg
	cfg.Provider = config.Provider(provider)
	cfg.APIKey = resolveKey(ap.opts.apiKey, provider, ap.settings)
	cfg.BaseURL = resolveBaseURL(ap.opts.baseURL, provider, dbSettings)
	if v := dbSettings.MaxTokens; v != 0 && ap.opts.maxTokens == 0 {
		cfg.MaxTokens = v
	}
	if v := dbSettings.Temperature; v != 0 && ap.opts.temperature == 0 {
		cfg.Temperature = v
	}
	m, err := config.BuildModel(cfg)
	if err != nil {
		return err
	}
	return ap.a.SetModel(m)
}

// memoryCloseGrace is how long a superseded memory engine stays open after a
// rebuild so in-flight prefetch/tool calls on it degrade to a logged error
// instead of racing a Close.
const memoryCloseGrace = 60 * time.Second

// rebuildMemory reconstructs the memory engine from live settings and swaps
// it into the agent. The old engine closes after a grace period so
// in-flight prefetch/tool calls on it degrade to a logged error instead of
// racing a Close.
func (ap *app) rebuildMemory() error {
	dbSettings := ap.settings.Load()
	eng, err := buildMemoryEngine(ap, dbSettings, ap.a.CurrentModel())
	if err != nil {
		return err
	}
	ap.memMu.Lock()
	old := ap.memEngine
	ap.memEngine = eng
	ap.memMu.Unlock()
	ap.a.SetMemoryEngine(eng)
	if old != nil {
		go func() {
			time.Sleep(memoryCloseGrace)
			_ = old.Close()
		}()
	}
	return nil
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

func resolveMode(mode string, skipPerms, acceptEdits, readOnly, plan bool) config.PermissionMode {
	switch {
	case skipPerms:
		return config.ModeBypass
	case acceptEdits:
		return config.ModeAcceptEdits
	case readOnly:
		return config.ModeReadOnly
	case plan:
		return config.ModePlan
	}
	if mode != "" {
		return config.PermissionMode(mode)
	}
	return config.ModeDefault
}

// resolveKey resolves the provider API key in order: CLI flag → environment
// variable → settings database (encrypted) → OAuth token.
func resolveKey(flagKey, provider string, st *settings.Store) string {
	if flagKey != "" {
		return flagKey
	}
	switch provider {
	case "openai":
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretOpenAIAPIKey); v != "" {
			return v
		}
	default:
		if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
			return v
		}
		if v := st.GetSecret(settings.SecretAnthropicAPIKey); v != "" {
			return v
		}
	}
	// Fall back to the OAuth token saved by `woca login`.
	return loadOAuthAccessToken()
}

// loadOAuthAccessToken reads the access_token from ~/.woca/credentials.json.
func loadOAuthAccessToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".woca", "credentials.json"))
	if err != nil {
		return ""
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return ""
	}
	return tok.AccessToken
}

// resolveBaseURL resolves the provider base URL in order: CLI flag →
// environment variable → settings database.
func resolveBaseURL(flagBaseURL, provider string, dbSettings settings.Settings) string {
	if flagBaseURL != "" {
		return flagBaseURL
	}
	switch provider {
	case "openai":
		if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
			return v
		}
		return dbSettings.OpenAIBaseURL
	default:
		if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" {
			return v
		}
		if v := os.Getenv("ANTHROPIC_API_URL"); v != "" {
			return v
		}
		return dbSettings.AnthropicBaseURL
	}
}

// loadHooksConfig merges the user (~/.woca/settings.json) and project
// (.woca/settings.json) hook configs.
func loadHooksConfig(workdir string) hooks.Config {
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".woca", "settings.json"))
	}
	paths = append(paths, filepath.Join(workdir, ".woca", "settings.json"))

	var merged hooks.Config
	for _, p := range paths {
		cfg, err := hooks.Load(p)
		if err != nil {
			continue
		}
		merged.PreToolUse = append(merged.PreToolUse, cfg.PreToolUse...)
		merged.PostToolUse = append(merged.PostToolUse, cfg.PostToolUse...)
		merged.UserPromptSubmit = append(merged.UserPromptSubmit, cfg.UserPromptSubmit...)
		merged.Stop = append(merged.Stop, cfg.Stop...)
	}
	return merged
}

// loadPermissions loads permission rules from settings.json files (user and
// project), merging them.
func loadPermissions(workdir string) *permissions.Rules {
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".woca", "settings.json"))
	}
	paths = append(paths, filepath.Join(workdir, ".woca", "settings.json"))

	var merged permissions.Rules
	for _, p := range paths {
		r, err := permissions.Load(p)
		if err != nil {
			continue
		}
		merged.Allow = append(merged.Allow, r.Allow...)
		merged.Deny = append(merged.Deny, r.Deny...)
		merged.Ask = append(merged.Ask, r.Ask...)
	}
	return &merged
}

// loadSkills loads skills from project and user skill directories.
func loadSkills(workdir string) []skills.Skill {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".woca", "skills"))
	}
	roots = append(roots,
		filepath.Join(workdir, ".woca", "skills"),
		filepath.Join(workdir, ".claude", "skills"),
	)
	var out []skills.Skill
	for _, root := range roots {
		if list, err := skills.Load(root); err == nil {
			out = append(out, list...)
		}
	}
	return out
}

// loadAgents loads custom subagent definitions from project and user dirs.
func loadAgents(workdir string) []agents.Definition {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".woca", "agents"))
	}
	roots = append(roots, filepath.Join(workdir, ".woca", "agents"))
	var out []agents.Definition
	for _, root := range roots {
		if list, err := agents.Load(root); err == nil {
			out = append(out, list...)
		}
	}
	return out
}

// connectMCP starts MCP servers and returns their tools.
// connectMCP starts MCP servers and returns their tools plus the clients
// (kept for between-turn tool refreshes).
func connectMCP(ctx context.Context, log zerolog.Logger, servers []string, st *settings.Store) ([]coretools.Tool, []*mcp.Client) {
	var out []coretools.Tool
	var clients []*mcp.Client
	for _, spec := range servers {
		name, command, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(command) == "" {
			log.Warn().Str("spec", spec).Msg("invalid --mcp value, expected name=command or name=http(s)://endpoint")
			continue
		}
		var client *mcp.Client
		if strings.HasPrefix(command, "http://") || strings.HasPrefix(command, "https://") {
			// OAuth-authorized servers carry their stored token (encrypted
			// in the secrets table by `woca mcp auth` / the McpAuth tool).
			var token func() string
			if st != nil {
				secret := mcp.McpTokenSecretPrefix + name + "_token"
				if st.HasSecret(secret) {
					token = func() string { return st.GetSecret(secret) }
				}
			}
			client = mcp.NewStreamableHTTPAuth(name, command, token)
		} else {
			client = mcp.NewStdio(name, "sh", "-c", command)
		}
		if client == nil {
			log.Warn().Str("name", name).Msg("failed to start MCP server")
			continue
		}
		if err := client.Initialize(ctx); err != nil {
			log.Warn().Str("name", name).Err(err).Msg("MCP initialize failed")
			continue
		}
		list, err := client.ListTools(ctx)
		if err != nil {
			log.Warn().Str("name", name).Err(err).Msg("MCP tools/list failed")
			continue
		}
		clients = append(clients, client)
		out = append(out, client.AsTools(name, list)...)
		out = append(out, client.AsResourceTools(name)...)
		out = append(out, client.AsPromptTools(name)...)
		log.Info().Str("name", name).Int("tools", len(list)).Msg("connected MCP server")
	}
	return out, clients
}

// refreshMCPTools re-lists tools from connected MCP servers, for the
// between-turn tool refresh (hot-added tools become usable next round).
func refreshMCPTools(ctx context.Context, log zerolog.Logger, clients []*mcp.Client) []coretools.Tool {
	var out []coretools.Tool
	for _, client := range clients {
		list, err := client.ListTools(ctx)
		if err != nil {
			continue
		}
		name := client.Name()
		out = append(out, client.AsTools(name, list)...)
		out = append(out, client.AsResourceTools(name)...)
		out = append(out, client.AsPromptTools(name)...)
	}
	return out
}

// turnTokenBudgetFromEnv resolves WOCA_TURN_TOKEN_BUDGET: the per-turn
// output-token budget for auto-continue (Claude Code's 500k feature).
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

// managedSettingsPath resolves the enterprise managed-settings file: flag →
// env → /etc/woca/managed-settings.json when present.
func managedSettingsPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("WOCA_MANAGED_SETTINGS"); v != "" {
		return v
	}
	if _, err := os.Stat("/etc/woca/managed-settings.json"); err == nil {
		return "/etc/woca/managed-settings.json"
	}
	return ""
}

// newMcpAuthRunner builds the OAuth runner shared by the McpAuth tool and
// `woca mcp auth`: tokens persist encrypted in the secrets table.
func newMcpAuthRunner(st *settings.Store) func(ctx context.Context, server string) error {
	return func(ctx context.Context, server string) error {
		if st == nil {
			return fmt.Errorf("settings store unavailable; run `woca mcp auth %s <endpoint>` with the database enabled", server)
		}
		for _, spec := range os.Args {
			_, command, ok := strings.Cut(spec, "=")
			if !ok {
				continue
			}
			if name, _, found := strings.Cut(spec, "="); found && name != server {
				continue
			}
			if !strings.HasPrefix(command, "http://") && !strings.HasPrefix(command, "https://") {
				continue
			}
			flow := mcp.OAuthFlow{
				ServerName: server,
				Endpoint:   command,
				Client:     &http.Client{Timeout: 15 * time.Second},
				OpenBrowser: func(u string) error {
					if os.Getenv("WOCA_NO_BROWSER") == "" {
						_ = openBrowser(u)
					}
					return nil
				},
				SaveToken: func(token string) error {
					return st.SetSecret(mcp.McpTokenSecretPrefix+server+"_token", token)
				},
			}
			return mcp.RunOAuth(ctx, flow)
		}
		return fmt.Errorf("MCP server %q is not configured with an https endpoint (start woca with --mcp %s=https://…)", server, server)
	}
}
