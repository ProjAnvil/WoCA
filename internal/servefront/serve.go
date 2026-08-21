// Package servefront implements the server-role frontends — the embedded
// web UI server (serve) and the background web-GUI daemon (web) — on top of
// the shared agentbuild composition root. Future server entries
// (woca-server, woca-gateway) import this package instead of the terminal
// frontends, which stay in the personal cmd/woca entry.
package servefront

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/msgplane"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/webd"
	"github.com/projanvil/woca/internal/webgui"
	webdist "github.com/projanvil/woca/web"
)

// Observability is the server-edition observability seam (design §12.3):
// OTel providers, /healthz + /metrics endpoints, and RED recording around
// the HTTP handler. It is implemented by internal/obs (OTel SDK) and linked
// only by server entry points; the personal edition passes nil and keeps
// the zero-dependency local telemetry sink (§5.2).
type Observability interface {
	// Install initializes providers and chains the OTel sink behind the
	// sink currently installed as the telemetry default (local logs stay
	// first, §6.5.3). Called before the agent build so startup spans are
	// captured.
	Install(ctx context.Context) error
	// Wrap adds /healthz + /metrics and RED recording around h. Applied to
	// the startup placeholder too, so K8s probes pass while the agent
	// builds (the same window /api/health already covers).
	Wrap(h http.Handler) http.Handler
	// Shutdown flushes and stops providers.
	Shutdown(ctx context.Context) error
	// SetRunState pushes the role's active/queued run counts into the
	// service gauges (internal/obs implements it).
	SetRunState(active, queued int64)
}

// NewServeCmd builds `woca serve`: run the embedded web UI server.
func NewServeCmd(opts *agentbuild.Options) *cobra.Command {
	return NewServeCmdWithObs(opts, nil)
}

// NewServeCmdWithObs builds the serve command with a server-edition
// observability assembly attached (nil factory keeps the personal-edition
// posture). The factory runs at command time so provider setup errors
// surface as normal command errors.
func NewServeCmdWithObs(opts *agentbuild.Options, makeObs func(ctx context.Context) (Observability, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the embedded web UI server (frontend + SSE API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			noBrowser, _ := cmd.Flags().GetBool("no-browser")
			authMode, _ := cmd.Flags().GetString("auth")
			var obsrv Observability
			if makeObs != nil {
				var err error
				if obsrv, err = makeObs(cmd.Context()); err != nil {
					return err
				}
			}
			return ServeWithObs(cmd.Context(), *opts, addr, noBrowser, authMode, obsrv)
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

// Serve runs the embedded web UI server (personal-edition posture: local
// telemetry only).
func Serve(ctx context.Context, opts agentbuild.Options, addr string, noBrowser bool, authMode string) error {
	return ServeWithObs(ctx, opts, addr, noBrowser, authMode, nil)
}

// ServeWithObs runs the embedded web UI server with an optional server-edition
// observability assembly (OTel SDK, /healthz + /metrics, RED metrics).
func ServeWithObs(ctx context.Context, opts agentbuild.Options, addr string, noBrowser bool, authMode string, obs Observability) error {
	// An unset log level disables zerolog entirely (logging.ParseLevel maps
	// "" to Disabled) — a server role running silent hides its watcher and
	// message-plane lifelines from operators. Default server roles to info.
	if opts.LogLevel == "" {
		opts.LogLevel = "info"
	}
	level, err := logging.ParseLevel(opts.LogLevel)
	if err != nil {
		return err
	}
	log := logging.NewConsole(os.Stderr, level)
	// Observability seam (Phase 2 §6.5): one structured line per tool/LLM/
	// skill span, correlated by tenant/run/session. The server edition
	// chains the OTel sink behind it (design §6.5.3).
	telemetry.SetDefault(telemetry.NewLocalSink(log))
	if obs != nil {
		if err := obs.Install(ctx); err != nil {
			return fmt.Errorf("observability: %w", err)
		}
		defer func() { _ = obs.Shutdown(context.WithoutCancel(ctx)) }()
	}

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
	if obs != nil {
		// Probes must answer from the first moment the port is bound.
		starting = obs.Wrap(starting)
	}
	handler.Store(&starting)
	hs := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load()).ServeHTTP(w, r)
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	// apikey auth is the multi-tenant posture: every tenant-sharing
	// subsystem must be remote or off. EnterpriseMemory forbids the
	// embedded ladyM engine (memory-engine must be remote or none).
	if authMode != "" && authMode != "local" {
		opts.EnterpriseMemory = true
	}
	// Queue dispatch (Phase 3 wiring): WOCA_RUN_DISPATCH=queue routes runs
	// to the shared run-queue for woca-worker instances to execute, instead
	// of in-process. RunsOffHost additionally opens the run surface to
	// remote tenants — a deployment form whose workers execute sandboxed
	// (WOCA_SANDBOX_ENDPOINT) may set both.
	dispatchQueue := strings.EqualFold(os.Getenv("WOCA_RUN_DISPATCH"), "queue")
	runsOffHost := dispatchQueue && strings.EqualFold(os.Getenv("WOCA_RUNS_OFF_HOST"), "true")
	opts.Sandbox = agentbuild.SandboxFromEnv()
	ap, err := agentbuild.BuildAgent(ctx, opts, log)
	if err != nil {
		_ = hs.Close()
		return err
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
	defer func() {
		for i := len(ap.Teardown) - 1; i >= 0; i-- {
			ap.Teardown[i]()
		}
	}()

	var native *server.NativeAuth
	if authMode != "" && authMode != "local" {
		authProv, err = newAPIKeyProvider(ap.Settings)
		if err != nil {
			_ = hs.Close()
			return err
		}
		native, err = newNativeAuth(ap.Settings, authProv)
		if err != nil {
			_ = hs.Close()
			return err
		}
	}

	sessionDir := ""
	if d := os.Getenv("WOCA_SESSION_DIR"); d != "" {
		// Shared-session deployments (queue dispatch with workers persisting
		// transcripts) point every role at one volume.
		sessionDir = d
	} else if d := session.DefaultDir(); d != "" {
		sessionDir = filepath.Join(d, "sessions")
	}
	// Message plane (design §5.4): the resolver reads the settings store
	// live, so console edits (mode / NATS URL / subject) apply to the very
	// next dispatch without a restart; the plane is a best-effort overlay
	// on top of the always-on PG queue.
	var plane *msgplane.Plane
	if ap.Settings != nil {
		plane = msgplane.New(func() msgplane.Config {
			c := ap.Settings.Load()
			return msgplane.Config{Mode: c.MsgPlaneMode, NATSURL: c.MsgPlaneNATSURL, Subject: c.MsgPlaneSubject}
		}, log)
		defer plane.Close()
	}
	var runLedger *runstate.Ledger
	if st := ap.Settings.DB(); st != nil {
		runLedger, err = runstate.New(st)
		if err != nil {
			_ = hs.Close()
			return fmt.Errorf("run ledger: %w", err)
		}
	}
	srv := server.New(server.Config{
		Auth:            authProv,
		NativeAuth:      native,
		RunLedger:       runLedger,
		DispatchToQueue: dispatchQueue,
		RunsOffHost:     runsOffHost,
		MsgPlane:        plane,
		Agent:           ap.Agent,
		Static:          webdist.DistFS(),
		Logger:          log,
		SessionDir:      sessionDir,
		WorkDir:         ap.Cfg.WorkDir,
		GrafanaURL:      os.Getenv("WOCA_GRAFANA_URL"),
		Settings:        ap.Settings,
		Memory:          ap.Memory,
		MemoryEngine:    ap.MemEngine,
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
	if obs != nil {
		real = obs.Wrap(real)
	}
	handler.Store(&real)

	url := "http://" + addr
	if !noBrowser {
		agentbuild.OpenBrowser(url)
	}
	fmt.Fprintf(os.Stderr, "woca web: %s\n", url)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Config-plane watcher (design §16.3/§16.7): the config service owns
	// settings writes now; rebuild the model/memory/debug-log hooks when
	// its writes move the generation fingerprint. The waiter's wake channel
	// turns the config plane's NATS push into an immediate rebuild; the
	// poll stays the safety net.
	var cfgWake <-chan struct{}
	if plane != nil {
		cfgWake = msgplane.NewWaiter(plane).Wake()
	}
	// Service run-state gauges (design §12.3): push the admission state
	// into the obs instruments on a short cadence so the series stay live.
	if obs != nil {
		go func() {
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					active, queued := srv.RunStateCounts()
					obs.SetRunState(int64(active), int64(queued))
				}
			}
		}()
	}

	go settingswatch.Watch(ctx, ap.Settings, ap.Settings.DB(), settingswatch.Identity{
		ServiceID: "srv-" + shortHost(), Role: "server",
	}, log, cfgWake, func() {
		if err := ap.RebuildModel(); err != nil {
			log.Warn().Err(err).Msg("model hot-swap after config change failed")
		}
		if err := ap.RebuildMemory(); err != nil {
			log.Warn().Err(err).Msg("memory hot-swap after config change failed")
		}
		if ap.Agent != nil && ap.Agent.Obs() != nil {
			ap.Agent.Obs().SetEnabled(ap.Settings.Load().DebugLog)
		}
	})

	return srv.ServeStarted(ctx, hs, serveErr)
}

// shortHost returns this machine's short hostname for service identities.
func shortHost() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
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

// newNativeAuth builds the native-client credential surface over the same
// tenant store as the API-key provider.
func newNativeAuth(st *settings.Store, keys authn.Provider) (*server.NativeAuth, error) {
	if st == nil {
		return nil, fmt.Errorf("native auth requires a settings database")
	}
	ts, err := tenant.NewStore(st.DB())
	if err != nil {
		return nil, fmt.Errorf("native auth: %w", err)
	}
	return server.NewNativeAuth(ts, keys), nil
}

// startingHandler serves the window between the port bind and the agent
// becoming ready: health keeps the webd readiness probe (and load
// balancers) happy, other API routes report 503, and the UI reloads itself
// until boot completes.
func startingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/health" {
		payload := map[string]any{"ok": true, "starting": true}
		if u := os.Getenv("WOCA_GRAFANA_URL"); u != "" {
			payload["grafana_url"] = u
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(payload)
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
