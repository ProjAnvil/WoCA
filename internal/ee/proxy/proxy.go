// Package proxy implements the executor-proxy (EE sandbox plane, spec
// §3.2): the stateless router standing in front of the sandbox executors.
// Five duties, no process state:
//
//   - bind: allocate-or-bind workspaces to executors (POST /internal/bindings,
//     idempotent on source_key; placement picks the freshest-least-loaded
//     executor from the config-plane heartbeats).
//   - route: every sandbox request carries X-Woca-Workspace; the binding
//     table resolves it to an executor, the executor's config ack resolves
//     to an address. The tenant always comes from the binding, never from
//     the request.
//   - flow control: a per-executor bounded concurrency semaphore with a
//     bounded FIFO wait queue, per-tenant queue quotas and a queue timeout —
//     overload sheds with 503 instead of buffering.
//   - forward: one httputil.ReverseProxy streams bytes both ways (tar.gz
//     archives are 512 MiB-class; bodies are never read into memory — the
//     create endpoint's small JSON body is the one buffered exception,
//     re-set onto the request after reading the workspace field). The
//     Authorization header passes through untouched: the executor's API-key
//     check is the last hop of the auth chain.
//   - degrade: a scan loop flips bindings to degraded when their executor's
//     ack leaves the 15s liveness window and back to active when it returns;
//     the request path additionally fail-closes on stale acks regardless of
//     the scan.
//
// The proxy registers with no registry and writes no ack of its own (it is
// stateless by design); its database is the shared woca database the entry
// point resolves (WOCA_DB, nacos:// included via wocadb's DSN seam).
package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/wocadb"
)

// WorkspaceHeader routes every sandbox request (CONTRACT.md §1): the client
// stamps it on each call, this proxy routes on it alone.
const WorkspaceHeader = "X-Woca-Workspace"

// ackLiveWindow bounds how old an executor's config ack may be before the
// proxy stops routing to it — the same 15s window the run dispatcher's gate
// uses (internal/server/runs.go); worker heartbeats refresh every 10s.
const ackLiveWindow = 15 * 1000 // ms

// degradedScanEvery is the binding-state scan cadence. A var so tests can
// tighten it; production runs 5s.
var degradedScanEvery = 5 * time.Second

// retryAfterRouteSec / retryAfterQueueSec back the Retry-After headers on
// routing and queue-pressure 503s. Routing failures clear when an executor
// acks again (within the 15s window), so the caller is told to come back
// after a scan interval; queue pressure is transient, so sooner.
const (
	retryAfterRouteSec = "5"
	retryAfterQueueSec = "1"
)

// sandboxBackendTimeoutHint mirrors the sandbox client's default per-call
// timeout (internal/platform/executor SandboxConfig.Timeout = 120s). The
// proxy cannot observe the real backend timeout — the queue timeout must
// merely stay below it or a dequeued request dies at the backend mid-run.
// New warns when the configuration is at or beyond the hint.
const sandboxBackendTimeoutHint = 120 * time.Second

// Defaults for Limits.
const (
	defaultPerExecutor  = 4
	defaultQueueLen     = 32
	defaultQueueTimeout = 30 * time.Second
)

// Limits bounds per-executor admission. Zero fields take the defaults in
// New.
type Limits struct {
	// PerExecutor is the concurrent in-flight request cap per executor.
	PerExecutor int
	// QueueLen is the per-executor FIFO wait-queue depth.
	QueueLen int
	// QueueTimeout bounds how long a request may wait in the queue; it must
	// stay below the backend's per-call timeout (see
	// sandboxBackendTimeoutHint).
	QueueTimeout time.Duration
	// TenantQuota caps one tenant's queued waiters per executor (fairness:
	// one tenant cannot flood the queue). Default max(2, QueueLen/4).
	TenantQuota int
}

// Config wires the proxy.
type Config struct {
	// DB is the shared woca database (bindings + config acks). Required.
	DB *wocadb.DB
	// Limits zero fields default in New.
	Limits Limits
	// Logger defaults to a disabled logger.
	Logger zerolog.Logger
}

// Proxy is the stateless workspace router. Construct with New, serve
// Handler, run Run alongside for the degraded scan.
type Proxy struct {
	db      *wocadb.DB
	limits  Limits
	log     zerolog.Logger
	queues  *queueSet
	rp      *httputil.ReverseProxy
	handler http.Handler
}

// New validates and defaults the configuration and assembles the proxy.
func New(cfg Config) *Proxy {
	if cfg.Limits.PerExecutor <= 0 {
		cfg.Limits.PerExecutor = defaultPerExecutor
	}
	if cfg.Limits.QueueLen <= 0 {
		cfg.Limits.QueueLen = defaultQueueLen
	}
	if cfg.Limits.QueueTimeout <= 0 {
		cfg.Limits.QueueTimeout = defaultQueueTimeout
	}
	if cfg.Limits.TenantQuota <= 0 {
		cfg.Limits.TenantQuota = max(2, cfg.Limits.QueueLen/4)
	}
	if cfg.Limits.QueueTimeout >= sandboxBackendTimeoutHint {
		// The backend timeout is not the proxy's to enforce (the caller's
		// client owns it); all New can do is flag the dangerous side.
		cfg.Logger.Warn().
			Dur("queue_timeout", cfg.Limits.QueueTimeout).
			Dur("backend_hint", sandboxBackendTimeoutHint).
			Msg("proxy: queue timeout at or beyond the sandbox client's default backend timeout — queued requests may time out at the backend mid-run")
	}
	p := &Proxy{
		db:     cfg.DB,
		limits: cfg.Limits,
		log:    cfg.Logger,
		queues: newQueueSet(cfg.Limits),
	}
	p.rp = p.buildReverseProxy()
	p.handler = p.buildHandler()
	return p
}

// buildHandler mounts the two surfaces: the allocate-or-bind control API
// and the sandbox contract forwarder.
func (p *Proxy) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/bindings", p.handleBind)
	mux.HandleFunc("/sandbox/", p.handleForward)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "unknown executor-proxy route")
	})
	return mux
}

// Handler returns the proxy's HTTP surface.
func (p *Proxy) Handler() http.Handler { return p.handler }

// Run drives the degraded scan until ctx ends (the entry point runs it in
// its own goroutine).
func (p *Proxy) Run(ctx context.Context) {
	tick := time.NewTicker(degradedScanEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.scanOnce()
		}
	}
}
