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
	"syscall"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/obs"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/projanvil/woca/internal/worker"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// obsAddrEnv is the probe/metrics listener address; "off" disables it.
const obsAddrEnv = "WOCA_OBS_ADDR"

func main() {
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

			// The settings database carries the runstate tables.
			opts := agentbuild.Options{LogLevel: logLevel}
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

			bus := eventbus.NewLocalWithRetention(1024)
			defer bus.Close()

			w := worker.New(worker.Config{
				Ledger:  ledger,
				Agent:   ap.Agent,
				Bus:     bus,
				Logger:  log,
				ID:      fmt.Sprintf("worker-%d", os.Getpid()),
				Metrics: workerMetrics(obsInst),
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
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
