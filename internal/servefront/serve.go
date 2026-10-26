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
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/agent"
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
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/webd"
	"github.com/projanvil/woca/internal/webgui"
	"github.com/projanvil/woca/internal/wocadb"
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
	// ObservabilityConsoleManaged marks editions whose observability
	// configuration is owned by the enterprise console even when their
	// auth posture is local-trust (the bridge: its users are enterprise
	// users, and the config plane delivers the plugin settings). Serve
	// rejects user writes of the observability plugin keys there.
	ObservabilityConsoleManaged bool
	// Nacos states the enterprise registry assembly (the full-stack
	// posture): when set, the serve path registers with nacos and takes
	// its configuration from the delivered pack — fail-closed, the
	// process does not start without the registry, and the PG settings
	// watcher is replaced by the pack subscription. Nil keeps the
	// personal/bridge posture (local settings watcher).
	Nacos *NacosAssembly

	// Static is the frontend filesystem the entry point injects: the
	// personal edition and the bridge embed web/dist; headless API roles
	// (woca-server) pass nil and serve a pageless API. This package never
	// imports the embedded frontend — the form split happens at assembly.
	Static fs.FS

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
	// Start runs after the main listener is up (optional; goroutine). The
	// obs instance is handed over so extra listeners mount the same
	// healthz/metrics plane and the HTTP span middleware — one observability
	// identity per process instead of per-listener ad-hoc setups.
	Start func(ctx context.Context, obs Observability, st *settings.Store, log zerolog.Logger)
	// RunLoop starts the edition's embedded run-execution loop (the
	// scheduler role: this process also claims dispatched runs off the
	// shared queue). It runs in its own goroutine until the serve context
	// ends, after the registry posture settled at startup's tail. Nil keeps
	// the personal posture — runs execute in-process, nothing claims the
	// queue.
	RunLoop func(ctx context.Context, deps RunLoopDeps)
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

	// SandboxEnabled reports live whether a sandbox backend is enabled
	// (server.Config.SandboxEnabled; design §10.2 M-3). The enterprise
	// entry injects a query against the materialized sandbox_backends
	// table — this package stays enterprise-free. Nil keeps the personal
	// posture: the process-level sandbox flag (WOCA_SANDBOX_ENDPOINT)
	// decides.
	SandboxEnabled func() bool

	// obs carries the built Observability from the cobra RunE into
	// ServeWithEdition (unexported; callers pass only the factories).
	obs Observability
}

// NacosAssembly is what a server-role entry point declares to join the
// full-stack registry: where nacos is, who this process is, and how to
// build the runtime (the entry closes over internal/ee/nacosrt; this
// package stays enterprise-free like the Observability seam).
type NacosAssembly struct {
	Server    string // nacos base URL, e.g. http://nacos:8848
	Service   string // nacos service name, e.g. woca-server
	Advertise string // optional host:port override (default: the serve addr)
	// Build constructs the runtime over the wiring the serve path
	// completes (settings store, ack identity, rebuild hooks). Required.
	Build func(a NacosAssembly, w NacosWiring, log zerolog.Logger) (NacosRuntime, error)
}

// NacosWiring is the serve path's half of the assembly.
type NacosWiring struct {
	St        *settings.Store
	DB        *wocadb.DB
	Identity  settingswatch.Identity
	OnChange  func()
	Advertise string
}

// NacosRuntime is the registry runtime's lifecycle as the serve path
// uses it: Start is the fail-closed startup gate; Loop owns heartbeat
// and config subscription until ctx ends.
type NacosRuntime interface {
	Start(ctx context.Context) error
	Loop(ctx context.Context)
}

// RunLoopDeps is what the serve path hands an edition's embedded run loop
// (Edition.RunLoop): the built agent, the settings store and the run ledger
// the loop claims from, the dispatch message plane (nil keeps the poll-only
// cadence), the shared session directory, the observability assembly and
// the logger. The loop implementation lives under internal/ee (the
// scheduler's embedded worker loop); this package stays enterprise-free —
// it only carries the pieces every posture agrees on.
type RunLoopDeps struct {
	Agent      *agent.Agent
	Settings   *settings.Store
	Ledger     *runstate.Ledger
	Plane      server.MsgPlane
	SessionDir string
	Obs        Observability
	Log        zerolog.Logger
	// NacosGeneration, when set, reports the registry runtime's last
	// applied config generation — the loop's capability heartbeat carries
	// it so the config plane sees this scheduler live at its current pack.
	// Nil (no registry posture) keeps the heartbeat's generation empty.
	NacosGeneration func() string
}

// NewServeCmdWithEdition builds the serve command with an enterprise
// edition assembly attached (see Edition). The entry point states its
// form explicitly — Static carries the frontend for UI roles (personal
// woca, bridge), nil keeps a pageless API for headless roles.
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
	tracing.SetDefault(tracing.NewLocalSink(log))
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

	hs, scheme, serveErr, swapHandler, err := bindEarly(addr, obs)
	if err != nil {
		return err
	}
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
		for _, v := range slices.Backward(ap.Teardown) {
			v()
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
		Auth: authProv,
		// A non-local auth plane (serve --auth apikey, the bridge edition)
		// owns the deployment: observability plugins are console-managed
		// there, and serve-side user writes are rejected.
		ObservabilityConsoleManaged: authProv != nil,
		ModelCatalog:                ed.ModelCatalog,
		ResolveModel:                ed.ResolveModel,
		AnonymousAPIPaths:           ed.AnonymousAPIPaths,
		HostGateExemptPrefixes:      ed.HostGateExemptPrefixes,
		StreamReauth:                ed.StreamReauth,
		NativeAuth:                  native,
		RunLedger:                   runLedger,
		DispatchToQueue:             dispatchQueue,
		RunsOffHost:                 runsOffHost,
		SandboxEnabled: func() bool {
			// Enterprise injection wins; the personal posture keeps the
			// process-level sandbox flag (WOCA_SANDBOX_ENDPOINT) as the
			// sandbox signal.
			if ed.SandboxEnabled != nil {
				return ed.SandboxEnabled()
			}
			return opts.Sandbox != nil
		},
		MsgPlane:     plane,
		Agent:        ap.Agent,
		Static:       ed.Static,
		Logger:       log,
		SessionDir:   sessionDir,
		WorkDir:      ap.Cfg.WorkDir,
		GrafanaURL:   os.Getenv("WOCA_GRAFANA_URL"),
		Settings:     ap.Settings,
		Memory:       ap.Memory,
		MemoryEngine: ap.MemEngine,
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
	// HTTP span plane (obs): one telemetry span per request, attributed to
	// the service's registry identity — the counterpart of the chat/tool
	// spans. Optional-interface seam: plain-core Observability (personal
	// edition, or an edition that opts out, e.g. the bridge) simply lacks
	// the method.
	if hm, ok := obs.(interface {
		HTTPMiddleware() func(http.Handler) http.Handler
	}); ok {
		real = hm.HTTPMiddleware()(real)
	}
	if obs != nil {
		real = obs.Wrap(real)
	}
	swapHandler.Store(real)
	if ed.Handler != nil {
		ed.Handler(real)
	}

	url := scheme + "://" + addr
	if !noBrowser && ed.Static != nil {
		agentbuild.OpenBrowser(url)
	}
	fmt.Fprintf(os.Stderr, "woca web: %s\n", url)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if ed.Start != nil {
		go ed.Start(ctx, obs, ap.Settings, log)
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

	rebuild := func() {
		if err := ap.RebuildModel(); err != nil {
			log.Warn().Err(err).Msg("model hot-swap after config change failed")
		}
		if err := ap.RebuildMemory(); err != nil {
			log.Warn().Err(err).Msg("memory hot-swap after config change failed")
		}
		if ap.Agent != nil && ap.Agent.Obs() != nil {
			ap.Agent.Obs().SetEnabled(ap.Settings.Load().DebugLog)
		}
	}
	// The registry runtime's generation (optional surface, the same
	// optional-interface pattern as the HTTP middleware seam above): the
	// embedded run loop's heartbeat carries it so the config plane sees
	// this scheduler at its current pack. Only the registry posture has
	// one.
	var nacosGen func() string
	if ed.Nacos != nil {
		// Full-stack posture: register with nacos (fail-closed — no
		// registry, no process) and take configuration from the
		// delivered pack. The PG settings watcher does not run.
		advertise := ed.Nacos.Advertise
		if advertise == "" {
			advertise = addr
		}
		rt, err := ed.Nacos.Build(*ed.Nacos, NacosWiring{
			St:        ap.Settings,
			DB:        ap.Settings.DB(),
			Identity:  settingswatch.Identity{ServiceID: "srv-" + shortHost(), Role: "server", Addr: advertise},
			OnChange:  rebuild,
			Advertise: advertise,
		}, log)
		if err != nil {
			return err
		}
		if err := rt.Start(ctx); err != nil {
			return err
		}
		if lg, ok := rt.(interface{ LastGeneration() string }); ok {
			nacosGen = lg.LastGeneration
		}
		go rt.Loop(ctx)
	} else {
		go settingswatch.Watch(ctx, ap.Settings, ap.Settings.DB(), settingswatch.Identity{
			ServiceID: "srv-" + shortHost(), Role: "server",
		}, log, cfgWake, rebuild)
	}

	// Embedded run loop seam (Edition.RunLoop): the edition's loop starts
	// here in its own goroutine, at startup's tail — after the registry
	// posture settled (the generation binding above is direct, and the
	// loop never starts on the fail-closed registry error path). Nil
	// keeps the personal posture: nothing claims the run queue here.
	if ed.RunLoop != nil {
		go ed.RunLoop(ctx, RunLoopDeps{
			Agent: ap.Agent, Settings: ap.Settings, Ledger: runLedger,
			Plane: plane, SessionDir: sessionDir, Obs: obs, Log: log,
			NacosGeneration: nacosGen,
		})
	}

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
