// woca-worker is the Phase 3 execution role (design §5.3/§13): it claims
// runs from the shared runstate ledger and executes them through the
// agent, mirroring events into run_events. Deploy alongside woca-server
// against the same database; neither links the interactive surfaces.
//
// Server governance baseline (design §12.3): /healthz + /metrics on a
// dedicated probe port (default :9091, --obs-addr), OTLP export when
// WOCA_OTEL_EXPORTER_OTLP_ENDPOINT is set, and run-lifecycle metrics
// (claims, finishes, duration).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/msgplane"
	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/worker"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// obsAddrEnv is the probe/metrics listener address; "off" disables it.
const obsAddrEnv = "WOCA_OBS_ADDR"

func main() {
	discovery.Install()
	root := newWorkerRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newWorkerRoot() *cobra.Command {
	var logLevel string
	root := &cobra.Command{
		Use:           "woca-worker",
		Short:         "woca execution worker (claims runs from the shared queue)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			obsAddr, _ := cmd.Flags().GetString("obs-addr")

			// Observability (design §12.3): local structured spans remain the
			// first sink; OTel export chains behind them.
			obsInst, err := obs.Setup(cmd.Context(), obs.FromEnv())
			if err != nil {
				return fmt.Errorf("observability: %w", err)
			}
			defer func() { _ = obsInst.Shutdown(context.WithoutCancel(cmd.Context())) }()
			obsInst.Install(cmd.Context())
			telemetry.SetDefault(telemetry.NewMultiSink(
				telemetry.NewLocalSink(log), obsInst.Sink()))

			probeSrv, err := serveProbes(obsAddr, obsInst)
			if err != nil {
				return err
			}
			if probeSrv != nil {
				defer func() { _ = probeSrv.Close() }()
			}

			// The settings database carries the runstate tables. The worker
			// is a server artifact (design §5.1): the embedded ladyM engine
			// is excluded — memory is remote (ladyM enterprise) or none,
			// enforced with the same enterprise gate woca-server uses.
			opts := agentbuild.Options{LogLevel: logLevel, EnterpriseMemory: true, Sandbox: agentbuild.SandboxFromEnv()}
			db, err := openWorkerDB(cmd, &opts)
			if err != nil {
				return err
			}
			defer db.Close()
			ledger, err := runstate.New(db)
			if err != nil {
				return fmt.Errorf("run ledger: %w", err)
			}

			ap, err := agentbuild.BuildAgent(cmd.Context(), opts, log)
			if err != nil {
				return err
			}
			defer ap.PluginMgr.Close()
			defer ap.Agent.Close()
			defer func() {
				for i := len(ap.Teardown) - 1; i >= 0; i-- {
					ap.Teardown[i]()
				}
			}()

			bus := eventbus.NewLocalWithRetention(1024)
			defer bus.Close()

			// Message plane (design §5.4): the same live settings resolver
			// the gateways use, so a console plane switch re-configures the
			// worker without a restart. Nil settings keep pure-PG polling.
			var idle worker.IdleWaiter
			if ap.Settings != nil {
				plane := msgplane.New(func() msgplane.Config {
					c := ap.Settings.Load()
					return msgplane.Config{Mode: c.MsgPlaneMode, NATSURL: c.MsgPlaneNATSURL, Subject: c.MsgPlaneSubject}
				}, log)
				defer plane.Close()
				idle = msgplane.NewWaiter(plane)
				defer idle.(*msgplane.Waiter).Close()
			}

			sessions := openWorkerSessions(log)
			var states worker.SessionStates
			if sessions != nil {
				states = &dbSessionStates{db: db}
			}

			w := worker.New(worker.Config{
				Ledger:   ledger,
				Agent:    ap.Agent,
				Bus:      bus,
				Logger:   log,
				ID:       workerID(),
				Metrics:  workerMetrics(obsInst),
				Sessions: sessions,
				States:   states,
				Idle:     idle,
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Registry + config delivery (full-stack posture): register with
			// nacos — WOCA_NACOS_URL is required, without the registry the
			// worker refuses to start (fail-closed, no fallback) — and take
			// configuration from the delivered pack (no PG settings
			// watcher in this posture).
			nacosBase := nacosrt.ServerFromEnv()
			if nacosBase == "" {
				return fmt.Errorf("worker requires WOCA_NACOS_URL (the nacos registry); refusing to start without it")
			}
			if obsAddr == "" || obsAddr == "off" {
				return fmt.Errorf("worker requires --obs-addr (the advertise port) when registering with nacos")
			}
			rt, err := nacosrt.New(nacosrt.Config{
				Server: nacosBase, Service: "woca-worker", Advertise: obsAddr,
				St: ap.Settings, DB: db,
				Identity: settingswatch.Identity{ServiceID: workerID(), Role: "worker", Addr: obsAddr},
				OnChange: func() {
					if err := ap.RebuildModel(); err != nil {
						log.Warn().Err(err).Msg("model hot-swap after config change failed")
					}
					if err := ap.RebuildMemory(); err != nil {
						log.Warn().Err(err).Msg("memory hot-swap after config change failed")
					}
					if ap.Agent != nil && ap.Agent.Obs() != nil {
						ap.Agent.Obs().SetEnabled(ap.Settings.Load().DebugLog)
					}
				},
			}, log)
			if err != nil {
				return err
			}
			if err := rt.Start(ctx); err != nil {
				return err
			}
			go rt.Loop(ctx)

			return w.Run(ctx)
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().String("db", "", "settings database: sqlite path, postgres:// DSN (env WOCA_DB)")
	root.Flags().String("obs-addr", defaultObsAddr(), "healthz/metrics listener address, or \"off\" (env WOCA_OBS_ADDR)")
	return root
}

// workerMetrics adapts the obs instruments into the worker's plain hooks.
func workerMetrics(o *obs.Obs) *worker.Metrics {
	wm := o.Worker()
	if wm == nil {
		return nil
	}
	return &worker.Metrics{
		OnClaim: func() { wm.Claimed.Add(context.Background(), 1) },
		OnFinish: func(status string, dur float64) {
			attrs := metric.WithAttributes(attribute.String("status", status))
			wm.Runs.Add(context.Background(), 1, attrs)
			wm.RunDur.Record(context.Background(), dur)
		},
	}
}

// defaultObsAddr resolves the probe listener default from the environment.
func defaultObsAddr() string {
	if v := os.Getenv(obsAddrEnv); v != "" {
		return v
	}
	return ":9091"
}

// serveProbes binds the /healthz + /metrics listener; "off" or a failure to
// bind (addr in use on a co-located sidecar port) disables probing rather
// than killing the worker — run execution outranks metrics.
func serveProbes(addr string, o *obs.Obs) (*http.Server, error) {
	if addr == "" || addr == "off" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("obs listener %s: %w", addr, err)
	}
	srv := &http.Server{Handler: o.Handler()}
	go func() { _ = srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "woca-worker probes: %s\n", addr)
	return srv, nil
}

// openWorkerSessions opens the shared session transcript store (the same
// JSONL-behind-blobstore layout woca-server uses) when WOCA_SESSION_DIR
// points at a directory shared with the gateways. Nil disables transcript
// persistence (prompt-only history). Note: the listing index (index.json)
// stays gateway-owned — the worker updates transcripts only, so a gateway's
// session title/message-count refresh on its next own save.
func openWorkerSessions(log zerolog.Logger) worker.Sessions {
	dir := os.Getenv("WOCA_SESSION_DIR")
	if dir == "" {
		return nil
	}
	blobs, err := blobstore.NewLocal(dir)
	if err != nil {
		log.Warn().Str("dir", dir).Err(err).Msg("session store unavailable; runs execute with prompt-only history")
		return nil
	}
	return &blobSessions{blobs: blobs}
}

// blobSessions implements worker.Sessions over the shared transcript
// directory.
type blobSessions struct {
	blobs blobstore.Store
}

func (b *blobSessions) Load(id string) []messages.Message {
	msgs, err := session.LoadBlob(context.Background(), b.blobs, id+".jsonl")
	if err != nil {
		return nil
	}
	return msgs
}

func (b *blobSessions) Save(id string, msgs []messages.Message, _ string) error {
	return session.SaveBlob(context.Background(), b.blobs, id+".jsonl", msgs)
}

// dbSessionStates implements worker.SessionStates over the shared
// session_state table (the same rows the gateways' listings read).
type dbSessionStates struct {
	db *wocadb.DB
}

func (s *dbSessionStates) MarkRunning(sessionID, runID string) {
	_ = s.db.SetSessionState(wocadb.SessionState{
		SessionID: sessionID,
		Status:    wocadb.SessionRunning,
		RunID:     runID,
		Owner:     fmt.Sprintf("%d@worker", os.Getpid()),
		UpdatedAt: wocadb.Now(),
	})
}

func (s *dbSessionStates) Clear(sessionID, runID string) {
	_, _ = s.db.ClearSessionState(sessionID, runID)
}

// openWorkerDB opens the shared settings/runstate database.
func openWorkerDB(cmd *cobra.Command, opts *agentbuild.Options) (*wocadb.DB, error) {
	target, _ := cmd.Flags().GetString("db")
	if target == "" {
		target = os.Getenv("WOCA_DB")
	}
	if target == "" || target == "none" {
		return nil, fmt.Errorf("the worker requires a shared database (set --db or WOCA_DB)")
	}
	db, err := wocadb.Open(target)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("the worker requires a database")
	}
	return db, nil
}

// workerID mints the lease identity: hostname + pid. The pid alone is not
// unique in containers (every container is pid 1 — the live stack had two
// workers both leasing as "worker-1", making ledger owner rows ambiguous).
func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return fmt.Sprintf("worker-%s-%d", host, os.Getpid())
}
