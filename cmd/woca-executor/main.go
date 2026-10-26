// woca-executor is the EE sandbox execution plane's container role
// (design §5.3, spec §3.2): it serves the contract v2 sandbox endpoints
// over a container Backend, owns the host-side workspace directories,
// and heartbeats its identity into the shared config-ack table so the
// stateless executor-proxy can route workspaces to it.
//
// Assembly (design §10.2 M-4):
//
//   - config plane: nacosrt Runtime (Service woca-executor, Role
//     executor) applies the delivered pack; the ParamsSource caches the
//     materialized enabled-backend row and OnChange refreshes it, so
//     image/runtime/network changes apply without a restart.
//   - heartbeat: the worker package's AckHeartbeat loop rewrites this
//     executor's ack row every 10s — generation from the last applied
//     pack, the "sandbox" capability exactly while a backend is enabled,
//     load from the live sandbox registry — which is both the proxy's
//     15s liveness signal and the run gate's dispatch precondition.
//   - backend: the docker driver behind the dockerlab build tag (see
//     backend_factory.go for the build matrix); WOCA_DEV_FAKE_BACKEND=1
//     opts into the in-process fake for development, loudly.
//
// The executor registers with nacos (WOCA_NACOS_URL required — fail
// closed, like the worker) and answers probes on --obs-addr.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/executor"
	nacosrt "github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/worker"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// Environment: probe listener (worker's convention), the hardening
// posture, the development-only fake backend, and the externally
// routable advertise address (host:port the proxy should dial; defaults
// to this host's name plus the --listen port).
const (
	obsAddrEnv       = "WOCA_OBS_ADDR"
	hardeningEnv     = "WOCA_SANDBOX_HARDENING"
	fakeBackendEnv   = "WOCA_DEV_FAKE_BACKEND"
	advertiseAddrEnv = "WOCA_EXECUTOR_ADVERTISE"
	workspaceRootEnv = "WOCA_WORKSPACE_ROOT"
	listenEnv        = "WOCA_EXECUTOR_LISTEN"
)

func main() {
	discovery.Install()
	root := newExecutorRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newExecutorRoot() *cobra.Command {
	var logLevel string
	root := &cobra.Command{
		Use:           "woca-executor",
		Short:         "woca sandbox executor (contract v2 sandbox service over a container backend)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			obsAddr, _ := cmd.Flags().GetString("obs-addr")

			obsInst, err := obs.Setup(cmd.Context(), obs.FromEnv())
			if err != nil {
				return fmt.Errorf("observability: %w", err)
			}
			defer func() { _ = obsInst.Shutdown(context.WithoutCancel(cmd.Context())) }()
			_ = obsInst.Install(cmd.Context())

			probeStop, err := serveProbes(cmd.Context(), obsAddr, log, obsInst)
			if err != nil {
				return err
			}
			defer probeStop()

			// The shared database carries the materialized sandbox row,
			// the settings secrets (the API key), and the ack table.
			db, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			st, err := settings.OpenFromDB(db)
			if err != nil {
				return fmt.Errorf("settings store: %w", err)
			}

			// Parameters (design §10.2 M-4): the enabled backend row is
			// the authority; OnChange (the pack hook below) refreshes.
			params := executor.NewParamsSource(db)
			if err := params.LastError(); err != nil {
				log.Warn().Err(err).Msg("executor: initial params load failed (disabled until the first pack applies)")
			}

			// The container backend (build matrix in backend_factory.go).
			dockerHost, _ := cmd.Flags().GetString("docker-host")
			backend, err := newBackend(dockerHost, log)
			if err != nil {
				return err
			}

			// The service: workspace store over --workspace-root, the
			// live options snapshot, labels carrying this executor's
			// identity, and the enabled backend's API key.
			workspaceRoot, _ := cmd.Flags().GetString("workspace-root")
			listen, _ := cmd.Flags().GetString("listen")
			maxConcurrent, _ := cmd.Flags().GetInt("max-concurrent")
			hardening, err := hardeningFromEnv()
			if err != nil {
				return err
			}
			id := executorID()
			cfg := executor.ServiceConfig{
				Backend:       backend,
				Store:         executor.NewWorkspaceStore(workspaceRoot, st.Load().SandboxArchiveMaxBytes),
				MaxConcurrent: maxConcurrent,
				Logger:        log,
				Hardening:     hardening,
				Options:       func() nacosrt.BackendOptions { return params.Snapshot().Options },
				Labels:        labelsFor(id),
				APIKey:        apiKeyFor(db, st, params, log),
			}
			svc, ok := executor.NewService(cfg).(*executor.Service)
			if !ok {
				return fmt.Errorf("executor service: unexpected concrete type")
			}
			defer func() { _ = svc.Close() }()

			// Startup sweep: remove the labeled leftovers a crashed
			// predecessor of this (stable) identity left behind. Also the
			// daemon-reachability gate: an unreachable docker socket is a
			// configuration error, not a degraded mode.
			if err := sweepLabeledSandboxes(backend, id, log); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Registry + config delivery: fail closed without nacos.
			nacosBase := nacosrt.ServerFromEnv()
			if nacosBase == "" {
				return fmt.Errorf("executor requires WOCA_NACOS_URL (the nacos registry); refusing to start without it")
			}
			if obsAddr == "" || obsAddr == "off" {
				return fmt.Errorf("executor requires --obs-addr (the advertise port) when registering with nacos")
			}
			advertiseFlag, _ := cmd.Flags().GetString("advertise")
			advertise := advertiseAddr(advertiseFlag, listen)
			rt, err := nacosrt.New(nacosrt.Config{
				Server: nacosBase, Service: "woca-executor", Advertise: advertise,
				St: st, DB: db,
				Identity: settingswatch.Identity{
					ServiceID: id, Role: "executor", Addr: "http://" + advertise,
				},
				OnChange: params.Refresh,
			}, log)
			if err != nil {
				return err
			}
			if err := rt.Start(ctx); err != nil {
				return err
			}
			go rt.Loop(ctx)

			// Ack heartbeat: the same loop shape the worker uses —
			// Role=executor, generation from the applied pack, the
			// "sandbox" capability while a backend is enabled, load from
			// the live registry. A bare worker.New carries it: the loop
			// needs only the identity, database and logger.
			hb := worker.New(worker.Config{DB: db, Logger: log, ID: id})
			go hb.AckHeartbeat(ctx, 0, executor.AckFunc(
				id, "http://"+advertise, rt.LastGeneration, params, func() int64 { return int64(svc.ActiveRuns()) },
			))

			contractSrv, err := serveContract(ctx, listen, svc)
			if err != nil {
				return err
			}

			<-ctx.Done()
			log.Info().Msg("executor: shutting down")
			shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = contractSrv.Shutdown(shutCtx) // svc.Close (deferred) force-removes live containers
			return nil
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().String("workspace-root", defaultWorkspaceRoot(), "host directory holding sandbox workspaces (env WOCA_WORKSPACE_ROOT)")
	root.Flags().String("listen", defaultListen(), "contract v2 sandbox service listener (env WOCA_EXECUTOR_LISTEN)")
	root.Flags().String("advertise", "", "host:port the proxy dials this executor at (env WOCA_EXECUTOR_ADVERTISE; default: this host's name + the listen port)")
	root.Flags().String("docker-host", "", "docker daemon address for the dockerlab backend (\"\" = the default unix socket)")
	root.Flags().Int("max-concurrent", 0, "concurrent live sandbox cap (0 = unlimited)")
	root.Flags().String("db", "", "settings database: sqlite path, postgres:// or nacos:// DSN (env WOCA_DB)")
	root.Flags().String("obs-addr", defaultObsAddr(), "healthz/metrics listener address, or \"off\" (env WOCA_OBS_ADDR)")
	return root
}

// newBackend resolves the container backend: the explicit development
// fake (WOCA_DEV_FAKE_BACKEND=1 — a loud WARN, never silent), the
// dockerlab-registered docker factory, or a fail-closed error in plain
// builds without the development switch.
func newBackend(dockerHost string, log zerolog.Logger) (executor.Backend, error) {
	if os.Getenv(fakeBackendEnv) == "1" {
		log.Warn().
			Str("backend", "fake").
			Msg("WOCA_DEV_FAKE_BACKEND=1: running the in-process fake sandbox backend — DEVELOPMENT ONLY, sandboxes are NOT isolated containers, never use this in production")
		return executor.NewFakeBackend()
	}
	if backendFactory == nil {
		return nil, errors.New("no container backend: build with -tags dockerlab for the docker driver, or set WOCA_DEV_FAKE_BACKEND=1 for the development fake (never production)")
	}
	return backendFactory(dockerHost)
}

// hardeningFromEnv parses WOCA_SANDBOX_HARDENING (full|minimal; empty =
// full — the fail-closed zero value).
func hardeningFromEnv() (executor.Hardening, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(hardeningEnv)))
	switch v {
	case "", "full":
		return executor.HardeningFull, nil
	case "minimal":
		return executor.HardeningMinimal, nil
	default:
		return 0, fmt.Errorf("%s must be full or minimal, got %q", hardeningEnv, v)
	}
}

// executorID mints the executor's identity: executor-<hostname>.
// Deliberately stable across restarts, unlike the worker's pid-suffixed
// lease identity: workspace bindings pin this id (the proxy routes a
// workspace to its bound executor's ack row — a restarted executor must
// answer to the same name or its bindings degrade forever), and labeled
// containers carry it (the startup sweep collects a crashed
// predecessor's leftovers). Two executors cannot share one workspace
// root or daemon anyway, so per-host uniqueness is the deployment's
// invariant, not the pid's.
func executorID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return "executor-" + host
}

// labelsFor builds the labels stamped on every created sandbox (design
// §10.3): the sandbox marker (/internal/sandboxes and the labeled GC
// filter on it), the workspace routing identity when routed, and this
// executor's stable id. woca.run stays off — the service knows the
// workspace at create time, not the owning run; run correlation lives
// in the binding source key and the run rows.
func labelsFor(executorID string) func(workspaceID string) map[string]string {
	return func(workspaceID string) map[string]string {
		labels := map[string]string{
			executor.LabelSandbox:  "1",
			executor.LabelExecutor: executorID,
		}
		if workspaceID != "" {
			labels[executor.LabelWorkspace] = workspaceID
		}
		return labels
	}
}

// apiKeyFor resolves the executor's bearer key at boot: the enabled
// backend's secret (sandbox-api-key:<backend-id>). EE runs a single
// enabled backend; with none enabled the key is empty — the service
// WARNs (the executor then trusts its fronting network, design §9.2).
// A key landed or rotated later applies on restart (the pack OnChange
// refresh re-arms the params half live).
func apiKeyFor(db *wocadb.DB, st *settings.Store, params *executor.ParamsSource, log zerolog.Logger) string {
	if !params.Snapshot().Enabled {
		log.Warn().Msg("executor: no enabled sandbox backend — serving unauthenticated (empty API key); enabling a backend and restarting arms the key")
		return ""
	}
	row, err := nacosrt.EnabledSandbox(db.DB)
	if err != nil || row == nil {
		log.Warn().Err(err).Msg("executor: enabled backend unreadable at key resolution — serving unauthenticated")
		return ""
	}
	return st.GetSecret("sandbox-api-key:" + row.ID)
}

// sweepLabeledSandboxes removes every sandbox container this executor
// identity created (label woca.executor=<id>): at boot the in-memory
// registry is empty, so anything still labeled is a crashed
// predecessor's leftover. This is the startup half of the labeled GC
// (Task 18 adds the periodic pass); it doubles as the daemon gate — an
// unreachable socket fails the start rather than surfacing mid-run.
func sweepLabeledSandboxes(backend executor.Backend, executorID string, log zerolog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	leftovers, err := backend.List(ctx, map[string]string{
		executor.LabelSandbox:  "1",
		executor.LabelExecutor: executorID,
	})
	if err != nil {
		return fmt.Errorf("executor: startup sweep: docker daemon unreachable or unreadable: %w", err)
	}
	for _, c := range leftovers {
		if err := c.Remove(ctx, true); err != nil {
			log.Warn().Err(err).Str("sandbox", c.ID()).Msg("executor: startup sweep: leftover removal failed (the periodic GC retries)")
		}
	}
	if len(leftovers) > 0 {
		log.Info().Int("count", len(leftovers)).Msg("executor: startup sweep removed leftover sandbox containers")
	}
	return nil
}

// advertiseAddr resolves the externally routable host:port the proxy
// dials: the explicit --advertise / WOCA_EXECUTOR_ADVERTISE (scheme
// tolerated, missing port taken from --listen), or this host's name
// plus the listen port. The value feeds both the nacos registration
// and the ack row's Addr (the proxy routes on "http://" + this).
func advertiseAddr(advertise, listen string) string {
	if v := strings.TrimSpace(os.Getenv(advertiseAddrEnv)); v != "" {
		advertise = v
	}
	advertise = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(advertise), "http://"), "https://")
	if advertise == "" {
		host := "localhost"
		if h, err := os.Hostname(); err == nil && h != "" {
			if i := strings.IndexByte(h, '.'); i > 0 {
				h = h[:i]
			}
			host = h
		}
		advertise = host
	}
	if _, _, err := net.SplitHostPort(advertise); err != nil {
		// No port (or a bare host): append the listen port.
		if _, port, perr := net.SplitHostPort(listen); perr == nil && port != "" {
			advertise = net.JoinHostPort(advertise, port)
		} else if _, port, perr := net.SplitHostPort(defaultListen()); perr == nil {
			advertise = net.JoinHostPort(advertise, port)
		}
	}
	return advertise
}

// defaultObsAddr resolves the probe listener default from the environment
// (:9094 — the next slot in the probe series after worker :9092 and
// config :9093).
func defaultObsAddr() string {
	if v := os.Getenv(obsAddrEnv); v != "" {
		return v
	}
	return ":9094"
}

// defaultWorkspaceRoot resolves the workspace root default from the
// environment (/data/workspaces — the compose volume mount point).
func defaultWorkspaceRoot() string {
	if v := os.Getenv(workspaceRootEnv); v != "" {
		return v
	}
	return "/data/workspaces"
}

// defaultListen resolves the contract listener default from the
// environment (:8093 — the sandbox data plane's first port in the
// server-plane 809x block after gw :8090 and ops :8091).
func defaultListen() string {
	if v := os.Getenv(listenEnv); v != "" {
		return v
	}
	return ":8093"
}

// serveProbes binds the /healthz + /metrics listener through webfx (the
// worker's probe posture: same request treatment as every woca surface;
// "off" disables; a bind failure is fatal).
func serveProbes(ctx context.Context, addr string, log zerolog.Logger, o *obs.Obs) (stop func(), err error) {
	if addr == "" || addr == "off" {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("obs listener %s: %w", addr, err)
	}
	r := webfx.New(webfx.Options{Logger: log})
	r.UseDefaults()
	r.Handle("/", o.Handler())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Serve(ctx, ln)
	}()
	fmt.Fprintf(os.Stderr, "woca-executor probes: %s\n", addr)
	return func() {
		_ = ln.Close()
		<-done
	}, nil
}

// serveContract binds the contract v2 sandbox service (all 16 endpoints
// plus /internal/sandboxes) on the configured listener.
func serveContract(ctx context.Context, addr string, h http.Handler) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("contract listener %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "woca-executor contract: %s\n", addr)
	return srv, nil
}

// openDB opens the shared settings/ack database (required).
func openDB(cmd *cobra.Command) (*wocadb.DB, error) {
	target, _ := cmd.Flags().GetString("db")
	if target == "" {
		target = os.Getenv("WOCA_DB")
	}
	if target == "" || target == "none" {
		return nil, fmt.Errorf("the executor requires a shared database (set --db or WOCA_DB)")
	}
	db, err := wocadb.Open(target)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("the executor requires a database")
	}
	return db, nil
}
