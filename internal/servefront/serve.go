// Package servefront implements the server-role frontends — the embedded
// web UI server (serve) and the background web-GUI daemon (web) — on top of
// the shared agentbuild composition root. Future server entries
// (woca-server, woca-gateway) import this package instead of the terminal
// frontends, which stay in the personal cmd/woca entry.
package servefront

import (
	"context"
	"crypto/tls"
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

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/fsb"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/webd"
	"github.com/projanvil/woca/internal/webgui"
	"github.com/projanvil/woca/internal/wocadb"
	webdist "github.com/projanvil/woca/web"
)

// Observability is the enterprise-edition observability seam (design §12.3):
// OTel providers, /healthz + /metrics endpoints, and RED recording around
// the HTTP handler. It is implemented by internal/ee/obs (OTel SDK) and linked
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
	// service gauges (internal/ee/obs implements it).
	SetRunState(active, queued int64)
}

// Edition bundles the enterprise subsystems a server-role entry point
// (woca-server) attaches to the shared serve path. Every field is
// optional; the personal edition (cmd/woca) runs with the zero Edition and
// each nil keeps the corresponding personal-edition posture. The
// enterprise implementations live under internal/ee — this package (and
// the whole core tree) never imports them.
type Edition struct {
	// Obs builds the observability assembly at command time, so provider
	// setup errors surface as normal command errors. Nil keeps the
	// zero-dependency local telemetry sink (§5.2).
	Obs func(ctx context.Context) (Observability, error)
	// Authn builds the API-key authn provider plus the native-client
	// credential surface over the settings store (the multi-tenant
	// posture). Required for --auth apikey; nil makes that mode an error.
	Authn func(st *settings.Store) (authn.Provider, server.NativeAuth, error)
	// Plane builds the run-dispatch message plane reading its config live
	// from the settings store, including the config-plane wake channel.
	// Nil keeps pure-PG dispatch (poll-only, no NATS overlay).
	Plane func(st *settings.Store, log zerolog.Logger) server.MsgPlane
	// FS overrides the workspace folder-browser service (product design
	// §4.1). Nil wires the default: roots from WOCA_FS_ROOTS (home when
	// unset), permissive — whole-filesystem browsing — only on loopback
	// binds (§4.3: the loopback posture keeps today's local behavior).
	FS *fsb.Service
	// Routes mounts edition-specific routes onto the API mux after the
	// authn provider is built (the bridge login/logout routes need it).
	// Nil mounts nothing.
	Routes func(mux *http.ServeMux, provider authn.Provider)
	// Handler receives the fully-built server handler (auth middleware
	// and all) once the agent build completes — the bridge's scenario-
	// four tunnel executes edge-parked client requests against exactly
	// this stack. Nil ignores it.
	Handler func(h http.Handler)
	// OnSettings receives the settings store right before the server is
	// assembled — in every auth posture (local included), so edition
	// subsystems that are not auth-dependent (the bridge's cloud-access
	// channel) exist even on loopback-trust bridges. Nil ignores it.
	OnSettings func(st *settings.Store)
	// PreBuildConfig runs inside the agent build, after the settings store
	// opens but before any setting is read or any build-time connection
	// opens — the bridge installs its managed overlay here with one
	// blocking initial config pull, so config-delivered storage/memory
	// coordinates are effective when the backends build. The hook's own
	// failures are logged, never fatal. Nil does nothing.
	PreBuildConfig func(ctx context.Context, st *settings.Store, log zerolog.Logger)
	// AnonymousAPIPaths / HostGateExemptPrefixes declare the form's
	// bootstrap exchanges and session surfaces (see server.Config). The
	// entry point that assembles the edition states them next to the
	// routes it mounts; core and servefront stay vocabulary-free.
	AnonymousAPIPaths      []string
	HostGateExemptPrefixes []string
	// StreamReauth revalidates open streams' credentials (revocation
	// closing; see server.Config). Entry points whose auth provider can
	// distinguish revocations supply it.
	StreamReauth func(credential string) error
	// Start runs edition-specific background loops once the agent build
	// finished and the settings store is open (the bridge's gateway
	// channel: enrollment, config pull, heartbeats). It runs in its own
	// goroutine until the serve context ends; nil does nothing.
	Start func(ctx context.Context, st *settings.Store, log zerolog.Logger)
	// Audit receives sensitive-operation events (run creation, workspace
	// writes) with the acting identity — the bridge wires it to its local
	// audit trail (product design R5). Nil skips auditing.
	Audit func(actor, action, detail string)
	// ModelCatalog overrides the local preset list for deployments whose
	// authoritative model catalog lives behind another plane (nil = the
	// local database).
	ModelCatalog func(ctx context.Context) ([]wocadb.ModelPreset, error)
	// ResolveModel resolves a preferred model against the authoritative
	// catalog, returning the preset and its credential when authorized
	// (nil = resolve against the local database).
	ResolveModel func(ctx context.Context, id, provider, model string) (wocadb.ModelPreset, string, bool)

	// obs carries the built Observability from the cobra RunE into
	// ServeWithEdition (unexported; callers pass only the factories).
	obs Observability
}

// NewServeCmd builds `woca serve`: run the embedded web UI server.
func NewServeCmd(opts *agentbuild.Options) *cobra.Command {
	return NewServeCmdWithEdition(opts, Edition{})
}

// NewServeCmdWithEdition builds the serve command with an enterprise
// edition assembly attached (see Edition; the zero value keeps the
// personal-edition posture).
func NewServeCmdWithEdition(opts *agentbuild.Options, ed Edition) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the embedded web UI server (frontend + SSE API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")
			noBrowser, _ := cmd.Flags().GetBool("no-browser")
			authMode, _ := cmd.Flags().GetString("auth")
			if ed.Obs != nil {
				obsrv, err := ed.Obs(cmd.Context())
				if err != nil {
					return err
				}
				ed.obs = obsrv
			}
			return ServeWithEdition(cmd.Context(), *opts, addr, noBrowser, authMode, ed)
		},
	}
	cmd.Flags().String("addr", webgui.DefaultAddr, "listen address for the web server")
	cmd.Flags().Bool("no-browser", false, "do not open the browser on startup")
	cmd.Flags().String("auth", "", "authentication mode: local (default, trust the machine) or apikey (woca_ API keys, enterprise deployments; managed via `woca-tenant tenant`; env WOCA_AUTH)")
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
	return ServeWithEdition(ctx, opts, addr, noBrowser, authMode, Edition{})
}

// ServeWithEdition runs the embedded web UI server with an enterprise
// edition assembly attached (observability, API-key auth, message plane);
// the zero Edition keeps the personal-edition posture.
func ServeWithEdition(ctx context.Context, opts agentbuild.Options, addr string, noBrowser bool, authMode string, ed Edition) error {
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
	// skill span, correlated by tenant/run/session. The enterprise edition
	// chains the OTel sink behind it (design §6.5.3).
	telemetry.SetDefault(telemetry.NewLocalSink(log))
	obs := ed.obs
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
		if ed.Authn == nil {
			return fmt.Errorf("apikey auth requires an enterprise server binary (woca-server); the personal edition trusts the local machine")
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
	// Direct TLS (product design §5.3 path C): WOCA_TLS_CERT/WOCA_TLS_KEY
	// terminate TLS on the business listener itself — for public bridges
	// without a reverse proxy. (The reverse-proxy paths keep using plain
	// HTTP on loopback behind Caddy/Tailscale.)
	tlsCert, tlsKey := os.Getenv("WOCA_TLS_CERT"), os.Getenv("WOCA_TLS_KEY")
	scheme := "http"
	if tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			_ = hs.Close()
			return fmt.Errorf("tls: load keypair: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		})
		scheme = "https"
	}
	// Ready file (native client embedding): when WOCA_READY_FILE is set,
	// the bound URL lands there atomically right after listen — the
	// embedding process polls this file to discover the random port.
	if f := os.Getenv("WOCA_READY_FILE"); f != "" && scheme == "http" {
		_ = os.WriteFile(f, []byte("http://"+ln.Addr().String()), 0o600)
	}
	go func() { serveErr <- hs.Serve(ln) }()

	// apikey auth is the multi-tenant posture: every tenant-sharing
	// subsystem must be remote or off. EnterpriseMemory forbids the
	// embedded ladyM engine (memory-engine must be remote or none).
	//
	// This reads like edition knowledge but is a product invariant on a
	// shared flag, and it cannot ride the Edition.Authn seam: the option
	// must reach BuildAgent, while the authn factory (and thus any
	// provider-existence signal) only runs afterwards, on ap.Settings.
	// "local" is servefront's own flag vocabulary, not an edition term —
	// a static Edition capability would regress local-auth bridges that
	// legitimately keep the embedded engine.
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
	if ed.PreBuildConfig != nil {
		hook := ed.PreBuildConfig
		opts.OnSettingsReady = func(ctx context.Context, st *settings.Store) error {
			hook(ctx, st, log)
			return nil
		}
	}
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

	var native server.NativeAuth
	if authMode != "" && authMode != "local" {
		authProv, native, err = ed.Authn(ap.Settings)
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
	// Message plane (design §5.4): the enterprise edition injects the
	// plane through Edition.Plane — its resolver reads the settings store
	// live, so console edits (mode / NATS URL / subject) apply to the very
	// next dispatch without a restart; the plane is a best-effort overlay
	// on top of the always-on PG queue. Personal deployments keep the
	// poll-only PG queue.
	var plane server.MsgPlane
	if ed.Plane != nil && ap.Settings != nil {
		plane = ed.Plane(ap.Settings, log)
		defer plane.Close()
	}
	if ed.OnSettings != nil {
		ed.OnSettings(ap.Settings)
	}
	var runLedger *runstate.Ledger
	if st := ap.Settings.DB(); st != nil {
		runLedger, err = runstate.New(st)
		if err != nil {
			_ = hs.Close()
			return fmt.Errorf("run ledger: %w", err)
		}
	}
	fsSvc := ed.FS
	if fsSvc == nil {
		fsSvc = defaultFS(addr)
	}
	srv := server.New(server.Config{
		Auth:                   authProv,
		ModelCatalog:           ed.ModelCatalog,
		ResolveModel:           ed.ResolveModel,
		AnonymousAPIPaths:      ed.AnonymousAPIPaths,
		HostGateExemptPrefixes: ed.HostGateExemptPrefixes,
		StreamReauth:           ed.StreamReauth,
		NativeAuth:             native,
		RunLedger:              runLedger,
		DispatchToQueue:        dispatchQueue,
		RunsOffHost:            runsOffHost,
		MsgPlane:               plane,
		Agent:                  ap.Agent,
		Static:                 webdist.DistFS(),
		Logger:                 log,
		SessionDir:             sessionDir,
		WorkDir:                ap.Cfg.WorkDir,
		GrafanaURL:             os.Getenv("WOCA_GRAFANA_URL"),
		Settings:               ap.Settings,
		Memory:                 ap.Memory,
		MemoryEngine:           ap.MemEngine,
		RebuildMemory: func() error {
			return ap.RebuildMemory()
		},
		RebuildModel: func() error {
			return ap.RebuildModel()
		},
		FS:    fsSvc,
		Audit: ed.Audit,
		Extras: func(mux *http.ServeMux) {
			if ed.Routes != nil {
				ed.Routes(mux, authProv)
			}
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
	if ed.Handler != nil {
		ed.Handler(real)
	}

	url := scheme + "://" + addr
	if !noBrowser {
		agentbuild.OpenBrowser(url)
	}
	fmt.Fprintf(os.Stderr, "woca web: %s\n", url)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if ed.Start != nil {
		go ed.Start(ctx, ap.Settings, log)
	}

	// Config-plane watcher (design §16.3/§16.7): the config service owns
	// settings writes now; rebuild the model/memory/debug-log hooks when
	// its writes move the generation fingerprint. The waiter's wake channel
	// turns the config plane's NATS push into an immediate rebuild; the
	// poll stays the safety net.
	var cfgWake <-chan struct{}
	if plane != nil {
		cfgWake = plane.Wake()
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

// defaultFS wires the folder-browser service for a serve bind: loopback
// binds keep the permissive whole-filesystem posture (§4.3 — the local
// single-user experience stays unchanged); any wider bind jails browsing
// to WOCA_FS_ROOTS or the home directory.
func defaultFS(addr string) *fsb.Service {
	roots := []string{}
	if v := os.Getenv("WOCA_FS_ROOTS"); v != "" {
		roots = filepath.SplitList(v)
	}
	return fsb.New(roots, isLoopbackBind(addr))
}

func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":port" binds every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
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
