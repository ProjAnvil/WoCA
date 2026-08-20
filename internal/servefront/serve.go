// Package servefront implements the server-role frontends — the embedded
// web UI server (serve) and the background web-GUI daemon (web) — on top of
// the shared agentbuild composition root. Future server entries
// (woca-server, woca-gateway) import this package instead of the terminal
// frontends, which stay in the personal cmd/woca entry.
package servefront

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/webd"
	"github.com/projanvil/woca/internal/webgui"
	webdist "github.com/projanvil/woca/web"
)

// NewServeCmd builds `woca serve`: run the embedded web UI server.
func NewServeCmd(opts *agentbuild.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the embedded web UI server (frontend + SSE API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			noBrowser, _ := cmd.Flags().GetBool("no-browser")
			authMode, _ := cmd.Flags().GetString("auth")
			return Serve(cmd.Context(), *opts, addr, noBrowser, authMode)
		},
	}
	cmd.Flags().String("addr", webgui.DefaultAddr, "listen address for the web server")
	cmd.Flags().Bool("no-browser", false, "do not open the browser on startup")
	cmd.Flags().String("auth", "", "authentication mode: local (default, trust the machine) or apikey (woca_ API keys, managed via `woca tenant`; env WOCA_AUTH)")
	return cmd
}

// NewWebCmd builds `woca web start|stop|restart|status`: manage the web GUI
// as a background daemon (detached `woca serve`, pid/addr in ~/.woca/web.json,
// output in ~/.woca/web.log).
func NewWebCmd() *cobra.Command {
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

// Serve runs the embedded web UI server.
func Serve(ctx context.Context, opts agentbuild.Options, addr string, noBrowser bool, authMode string) error {
	level, err := logging.ParseLevel(opts.LogLevel)
	if err != nil {
		return err
	}
	log := logging.NewConsole(os.Stderr, level)

	if authMode == "" {
		authMode = os.Getenv("WOCA_AUTH")
	}
	var authProv authn.Provider
	if authMode != "" && authMode != "local" {
		switch authMode {
		case "apikey", "api-key", "api-keys", "keys":
		default:
			return fmt.Errorf("unknown --auth mode %q (want local or apikey)", authMode)
		}
	}

	// Bind and answer /api/health before the agent build: the build makes
	// network calls (MCP connect, embedding probe) that can hang for many
	// seconds under a VPN or a stalled endpoint, which used to leave the
	// port unbound and the webd daemon timed out on its 5s readiness
	// probe. Everything except health 503s until the real handler is
	// swapped in below.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	// atomic.Pointer, not atomic.Value: the swapped-in concrete handler
	// types differ (http.HandlerFunc vs *http.ServeMux) and atomic.Value
	// panics on inconsistent types.
	var handler atomic.Pointer[http.Handler]
	starting := http.Handler(http.HandlerFunc(startingHandler))
	handler.Store(&starting)
	hs := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load()).ServeHTTP(w, r)
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	ap, err := agentbuild.BuildAgent(ctx, opts, log)
	if err != nil {
		_ = hs.Close()
		return err
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()

	if authMode != "" && authMode != "local" {
		authProv, err = newAPIKeyProvider(ap.Settings)
		if err != nil {
			_ = hs.Close()
			return err
		}
	}

	sessionDir := ""
	if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	srv := server.New(server.Config{
		Auth:         authProv,
		Agent:        ap.Agent,
		Static:       webdist.DistFS(),
		Logger:       log,
		SessionDir:   sessionDir,
		WorkDir:      ap.Cfg.WorkDir,
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
	real := http.Handler(srv.Handler())
	handler.Store(&real)

	url := "http://" + addr
	if !noBrowser {
		agentbuild.OpenBrowser(url)
	}
	fmt.Fprintf(os.Stderr, "woca web: %s\n", url)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.ServeStarted(ctx, hs, serveErr)
}

// newAPIKeyProvider builds the API-key authn provider over the settings
// database (the same database the agent build opened, so tenant tables and
// credentials live next to settings).
func newAPIKeyProvider(st *settings.Store) (authn.Provider, error) {
	if st == nil {
		return nil, fmt.Errorf("api-key auth requires a settings database (set --db; it cannot be \"none\")")
	}
	ts, err := tenant.NewStore(st.DB())
	if err != nil {
		return nil, fmt.Errorf("api-key auth: %w", err)
	}
	return tenant.NewAPIKeyProvider(ts), nil
}

// startingHandler serves the window between the port bind and the agent
// becoming ready: health keeps the webd readiness probe (and load
// balancers) happy, other API routes report 503, and the UI reloads itself
// until boot completes.
func startingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true,"starting":true}`)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"starting"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta http-equiv="refresh" content="1"><title>woca starting…</title><p>woca is starting…</p>`)
}
