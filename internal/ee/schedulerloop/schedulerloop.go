// Package schedulerloop embeds the worker run loop inside the scheduler
// role (woca-scheduler) through the servefront RunLoop seam: the same claim →
// execute → finish cycle the standalone woca-worker drives, assembled from
// the pieces the serve path already owns — the built agent, the settings
// store's runstate ledger, the message plane's dispatch waiter, the shared
// session volume. The serve path calls RunLoop in its own goroutine; this
// package adapts servefront.RunLoopDeps into worker.Config and runs the
// loop until the serve context ends.
package schedulerloop

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/projanvil/woca/internal/ee/edition"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/worker"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/wocadb"
)

// ackHeartbeatEvery is the scheduler's config-ack refresh cadence — the
// same 10s window the worker role's heartbeat uses (design §10.2 M-4),
// comfortably inside the dispatcher's 15s live window.
const ackHeartbeatEvery = 10 * time.Second

// RunLoop adapts the servefront seam into the worker assembly and drives it
// until ctx ends. It is the Edition.RunLoop the scheduler entry injects:
// deps carry the serve path's own agent, settings store, run ledger and
// dispatch plane, so the embedded loop and the API surface share one
// process identity.
//
// Without a ledger (no settings database: the serve path leaves
// deps.Ledger nil) or without an agent there is nothing to claim or execute
// with — the loop declines to start instead of spinning on nil.
func RunLoop(ctx context.Context, deps servefront.RunLoopDeps) {
	log := deps.Log
	if deps.Ledger == nil || deps.Agent == nil {
		log.Info().Msg("schedulerloop: no run ledger; embedded run loop stays off")
		return
	}

	// The sandbox/config wiring rides the settings store when one is open;
	// a nil store (never in the serve path once a ledger exists, but this
	// is a public seam) keeps the no-database posture: no backend secrets,
	// no archive cap override, no ack heartbeat.
	var db *wocadb.DB
	maxArchive := int64(0)
	secretFor := func(string) string { return "" }
	if deps.Settings != nil {
		db = deps.Settings.DB()
		maxArchive = deps.Settings.Load().SandboxArchiveMaxBytes
		secretFor = func(backendID string) string {
			return deps.Settings.GetSecret("sandbox-api-key:" + backendID)
		}
	}

	sessions := worker.NewFileSessions(deps.SessionDir, log)
	var states worker.SessionStates
	if db != nil {
		states = worker.NewDBSessionStates(db)
	}

	w := worker.New(worker.Config{
		Ledger: deps.Ledger,
		Agent:  deps.Agent,
		// The loop's own event fanout (SSE tail reads from run_events via
		// the ledger mirror; the bus serves in-process subscribers).
		Bus:     eventbus.NewLocalWithRetention(1024),
		Logger:  log,
		ID:      schedulerID(),
		Metrics: metricsFrom(deps.Obs),
		// Session transcript persistence follows the shared session dir
		// (empty dir disables it — NewFileSessions' nil semantics); the
		// busy-state rows ride the shared database.
		Sessions: sessions,
		States:   states,
		// Park between empty claims on the plane's dispatch waiter (the
		// same NATS wake-up the standalone worker uses); nil keeps the
		// poll cadence.
		Idle:            edition.IdleWaiter(deps.Plane),
		DB:              db,
		SecretFor:       secretFor,
		MaxArchiveBytes: maxArchive,
	})

	// Config-plane heartbeat (design §10.2 M-4): rewrite this scheduler's
	// ack row on a ticker so a healthy-but-idle scheduler stays inside the
	// dispatcher's live window; the generation, when a registry runtime is
	// attached, reports the pack this process last applied.
	if db != nil {
		gen := deps.NacosGeneration
		go w.AckHeartbeat(ctx, ackHeartbeatEvery, func() wocadb.ConfigAck {
			ack := wocadb.ConfigAck{Role: "scheduler"}
			if gen != nil {
				ack.Generation = gen()
			}
			return ack
		})
	}

	if err := w.Run(ctx); err != nil {
		log.Error().Err(err).Msg("schedulerloop: run loop exited")
	}
}

// metricsFrom adapts the observability assembly's worker instruments into
// the loop's plain hooks — the embedded loop feeds the same claim /
// finish / duration series the standalone worker role does, so operators
// read one shape regardless of where runs execute. Editions without the
// surface (or a nil Obs) keep metrics off.
func metricsFrom(o servefront.Observability) *worker.Metrics {
	src, ok := o.(interface{ Worker() *obs.WorkerMetrics })
	if !ok {
		return nil
	}
	wm := src.Worker()
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

// schedulerID mints the loop's lease identity: hostname + pid. The pid
// alone is not unique in containers (every container is pid 1 — the live
// stack had two workers both leasing as "worker-1"); the hostname prefix
// keeps two schedulers on one host, or one per container, distinct.
func schedulerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return fmt.Sprintf("sched-%s-%d", host, os.Getpid())
}
