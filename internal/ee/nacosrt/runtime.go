// Runtime: the per-process nacosrt component a full-stack role assembles.
// Start is the fail-closed gate — registration and the initial config
// pack alignment both happen synchronously, and any failure aborts the
// process (by contract: a role that cannot reach nacos does not start).
// Loop then owns the heartbeat and the pack subscription until ctx ends.
package nacosrt

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// ConfigDataID is the delivered configuration pack's entry.
const ConfigDataID = "woca-config.pack"

// Cadence.
const (
	beatEvery     = 5 * time.Second
	listenTimeout = 30 * time.Second
	retryEvery    = 3 * time.Second
)

// Config states one role's runtime assembly. Service and Advertise are
// required; St/db/Identity/OnChange drive the config-pack subscription
// (nil St = subscribe disabled: the edge registers without materializing,
// its RawPack callback still sees every delivered pack).
type Config struct {
	Server    string // nacos base URL (http://nacos:8848)
	Service   string // nacos service name: woca-scheduler / woca-worker / woca-edge
	Advertise string // host:port this instance serves (host empty = autodetect)

	St       *settings.Store // materialization target (nil = no pack apply)
	DB       *wocadb.DB      // pack tables + config acks (nil = no ack)
	Identity settingswatch.Identity
	OnChange func()      // rebuild hooks after every applied pack
	RawPack  func(*Pack) // raw delivery for DB-less subscribers (edge)

	log zerolog.Logger
}

// Runtime is the assembled component. Build with New.
type Runtime struct {
	cfg  Config
	cli  *Client
	ip   string
	port int
	// pack is the last applied/received pack. Whole-pack swap semantics:
	// apply (Loop's subscription goroutine) publishes a complete new pack,
	// readers (LastGeneration — the scheduler/worker heartbeats, watchLoop's
	// md5 seed) load it — atomic pointer, never a torn or racy read.
	pack atomic.Pointer[Pack]
}

// New validates the assembly and prepares the runtime without touching
// the network.
func New(cfg Config, log zerolog.Logger) (*Runtime, error) {
	if cfg.Server == "" || cfg.Service == "" || cfg.Advertise == "" {
		return nil, fmt.Errorf("nacosrt: Server, Service and Advertise are all required (service %q)", cfg.Service)
	}
	cfg.log = log
	return &Runtime{cfg: cfg, cli: NewClient(cfg.Server)}, nil
}

// Start is the startup gate: it pings the registry, registers the
// instance, and — when a settings store is attached — pulls the config
// pack and materializes it before returning. Any error is fatal for the
// caller; there is no degraded posture.
func (r *Runtime) Start(ctx context.Context) error {
	if err := r.cli.Ping(ctx); err != nil {
		return fmt.Errorf("nacosrt %s: registry unreachable: %w", r.cfg.Service, err)
	}
	host, port, err := net.SplitHostPort(r.cfg.Advertise)
	if err != nil {
		return fmt.Errorf("nacosrt: advertise %q: %w", r.cfg.Advertise, err)
	}
	if r.ip = host; r.ip == "" {
		if r.ip, err = autodetectIP(); err != nil {
			return fmt.Errorf("nacosrt %s: %w", r.cfg.Service, err)
		}
	}
	if r.port, err = strconv.Atoi(port); err != nil {
		return fmt.Errorf("nacosrt: advertise port %q: %w", port, err)
	}
	meta := map[string]string{"service": r.cfg.Service}
	if r.cfg.Identity.Role != "" {
		meta["role"] = r.cfg.Identity.Role
	}
	if r.cfg.Identity.Addr != "" {
		meta["addr"] = r.cfg.Identity.Addr
	}
	if err := r.cli.Register(ctx, r.cfg.Service, r.ip, r.port, meta); err != nil {
		return fmt.Errorf("nacosrt %s: register failed: %w", r.cfg.Service, err)
	}
	r.cfg.log.Info().Str("service", r.cfg.Service).Str("instance", fmt.Sprintf("%s:%d", r.ip, r.port)).Msg("nacosrt: registered with nacos")

	// Initial config alignment: the pack must exist and must be applied
	// before the role serves. A missing pack means the stack was never
	// initialized (nacos-init publishes the empty pack) — refuse to run.
	if r.cfg.St != nil || r.cfg.RawPack != nil {
		content, err := r.cli.Get(ctx, "DEFAULT_GROUP", ConfigDataID)
		if err != nil {
			return fmt.Errorf("nacosrt %s: initial config pack: %w", r.cfg.Service, err)
		}
		pack, err := DecodePack(content)
		if err != nil {
			return fmt.Errorf("nacosrt %s: %w", r.cfg.Service, err)
		}
		if err := r.apply(ctx, pack); err != nil {
			return fmt.Errorf("nacosrt %s: initial pack apply: %w", r.cfg.Service, err)
		}
		r.cfg.log.Info().Str("generation", pack.Generation).Msg("nacosrt: initial config pack applied")
	}
	return nil
}

// Loop runs the heartbeat and the pack subscription until ctx ends, then
// deregisters (best effort — the ephemeral instance would lapse anyway).
func (r *Runtime) Loop(ctx context.Context) {
	go r.beatLoop(ctx)
	if r.cfg.St != nil || r.cfg.RawPack != nil {
		r.watchLoop(ctx)
	} else {
		<-ctx.Done()
	}
	deregCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.cli.Deregister(deregCtx, r.cfg.Service, r.ip, r.port)
}

func (r *Runtime) beatLoop(ctx context.Context) {
	t := time.NewTicker(beatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			known, err := r.cli.Beat(rctx, r.cfg.Service, r.ip, r.port)
			cancel()
			if err != nil {
				// The registry will mark the instance unhealthy after the
				// lapse window — surface it, keep serving; the console
				// shows the truth rather than a fabricated good state.
				r.cfg.log.Warn().Err(err).Msg("nacosrt: heartbeat failed (instance will go unhealthy in nacos)")
				continue
			}
			if !known {
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := r.cli.Register(rctx, r.cfg.Service, r.ip, r.port, nil)
				cancel()
				if err != nil {
					r.cfg.log.Error().Err(err).Msg("nacosrt: re-register after forgotten instance failed")
					continue
				}
				r.cfg.log.Info().Msg("nacosrt: re-registered (nacos had forgotten the instance)")
			}
		}
	}
}

func (r *Runtime) watchLoop(ctx context.Context) {
	md5 := ""
	if p := r.pack.Load(); p != nil {
		if raw, err := p.Encode(); err == nil {
			md5 = contentMD5(raw)
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		changed, err := r.cli.Listen(ctx, "DEFAULT_GROUP", ConfigDataID, md5, listenTimeout)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.cfg.log.Warn().Err(err).Msg("nacosrt: config listen failed; retrying")
			time.Sleep(retryEvery)
			continue
		}
		if !changed {
			continue
		}
		gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		content, err := r.cli.Get(gctx, "DEFAULT_GROUP", ConfigDataID)
		cancel()
		if err != nil {
			r.cfg.log.Error().Err(err).Msg("nacosrt: changed pack fetch failed; retrying")
			time.Sleep(retryEvery)
			continue
		}
		pack, err := DecodePack(content)
		if err != nil {
			r.cfg.log.Error().Err(err).Msg("nacosrt: delivered pack refused")
			continue
		}
		if err := r.apply(ctx, pack); err != nil {
			r.cfg.log.Error().Err(err).Msg("nacosrt: pack apply failed")
			continue
		}
		if raw, err := pack.Encode(); err == nil {
			md5 = contentMD5(raw)
		}
		r.cfg.log.Info().Str("generation", pack.Generation).Msg("nacosrt: config pack delivered; applied")
	}
}

// apply materializes a pack and runs the change hooks. The pieces are
// small on purpose: settings overlay + secrets, then the shared tables,
// then the ack — each independently readable.
func (r *Runtime) apply(ctx context.Context, pack *Pack) error {
	r.pack.Store(pack)
	if r.cfg.RawPack != nil {
		r.cfg.RawPack(pack)
	}
	if r.cfg.St == nil {
		return nil
	}
	r.cfg.St.SetManaged(pack.Settings)
	for name, value := range pack.Secrets {
		if err := r.cfg.St.SetSecret(name, value); err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
	}
	if r.cfg.DB != nil {
		if err := r.materializeTables(pack); err != nil {
			return err
		}
		r.ack(pack)
	}
	if r.cfg.OnChange != nil {
		r.cfg.OnChange()
	}
	return nil
}

// materializeTables upserts the pack's shared-table domains: managed
// skills/MCP (the operator-delivered set) and the gateway policy entries.
// The sandbox backends follow full-manifest semantics (design §10.2 M-2):
// rows absent from the pack are deleted, including a previously enabled
// one — the pack is the complete inventory, never a patch.
func (r *Runtime) materializeTables(pack *Pack) error {
	db := r.cfg.DB
	if err := ensureSandboxBackends(db); err != nil {
		return err
	}
	for _, s := range pack.Skills {
		if _, err := db.Exec(`INSERT INTO managed_skills (name, description, body, updated_at) VALUES (`+
			db.Dialect.Placeholder(0)+`, `+db.Dialect.Placeholder(1)+`, `+db.Dialect.Placeholder(2)+`, `+db.Dialect.Placeholder(3)+`)
			ON CONFLICT(name) DO UPDATE SET description = excluded.description, body = excluded.body, updated_at = excluded.updated_at`,
			s.Name, s.Description, s.Body, wocadb.Now()); err != nil {
			return fmt.Errorf("managed skill %s: %w", s.Name, err)
		}
	}
	for _, m := range pack.MCP {
		if _, err := db.Exec(`INSERT INTO managed_mcp (name, spec, updated_at) VALUES (`+
			db.Dialect.Placeholder(0)+`, `+db.Dialect.Placeholder(1)+`, `+db.Dialect.Placeholder(2)+`)
			ON CONFLICT(name) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`,
			m.Name, m.Spec, wocadb.Now()); err != nil {
			return fmt.Errorf("managed mcp %s: %w", m.Name, err)
		}
	}
	for k, v := range pack.Gateway {
		if _, err := db.Exec(`INSERT INTO gw_settings (key, value) VALUES (`+
			db.Dialect.Placeholder(0)+`, `+db.Dialect.Placeholder(1)+`)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("gateway setting %s: %w", k, err)
		}
	}
	for _, sb := range pack.Sandboxes {
		var enabled int64
		if sb.Enabled {
			enabled = 1
		}
		var probeOK any
		if sb.LastProbeOK {
			probeOK = 1
		} else if sb.LastProbeAt > 0 {
			probeOK = 0
		}
		var probeAt any
		if sb.LastProbeAt > 0 {
			probeAt = sb.LastProbeAt
		}
		// Options ride the row as JSON text (design §17): the executor
		// reads them from this materialized column.
		optsText, err := EncodeOptionsText(sb.Options)
		if err != nil {
			return fmt.Errorf("sandbox backend %s: %w", sb.ID, err)
		}
		if _, err := db.Exec(`INSERT INTO sandbox_backends
			(id, name, type, endpoint, timeout_ms, enabled, last_probe_at, last_probe_ok, last_probe_error, options, created_at, updated_at)
			VALUES (`+db.Dialect.Placeholder(0)+`, `+db.Dialect.Placeholder(1)+`, `+db.Dialect.Placeholder(2)+`, `+
			db.Dialect.Placeholder(3)+`, `+db.Dialect.Placeholder(4)+`, `+db.Dialect.Placeholder(5)+`, `+
			db.Dialect.Placeholder(6)+`, `+db.Dialect.Placeholder(7)+`, `+db.Dialect.Placeholder(8)+`, `+
			db.Dialect.Placeholder(9)+`, `+db.Dialect.Placeholder(10)+`, `+db.Dialect.Placeholder(11)+`)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, type = excluded.type, endpoint = excluded.endpoint,
				timeout_ms = excluded.timeout_ms, enabled = excluded.enabled, last_probe_at = excluded.last_probe_at,
				last_probe_ok = excluded.last_probe_ok, last_probe_error = excluded.last_probe_error,
				options = excluded.options, updated_at = excluded.updated_at`,
			sb.ID, sb.Name, sb.Type, sb.Endpoint, sb.TimeoutMs, enabled, probeAt, probeOK, sb.LastProbeError,
			optsText, wocadb.Now(), wocadb.Now()); err != nil {
			return fmt.Errorf("sandbox backend %s: %w", sb.ID, err)
		}
	}
	// Full-manifest cleanup: delete rows the pack no longer carries. An
	// empty manifest clears the table.
	if len(pack.Sandboxes) == 0 {
		if _, err := db.Exec(`DELETE FROM sandbox_backends`); err != nil {
			return fmt.Errorf("sandbox backend cleanup: %w", err)
		}
	} else {
		ph := make([]string, len(pack.Sandboxes))
		args := make([]any, len(pack.Sandboxes))
		for i, sb := range pack.Sandboxes {
			ph[i] = db.Dialect.Placeholder(i)
			args[i] = sb.ID
		}
		if _, err := db.Exec(`DELETE FROM sandbox_backends WHERE id NOT IN (`+strings.Join(ph, ", ")+`)`, args...); err != nil {
			return fmt.Errorf("sandbox backend cleanup: %w", err)
		}
	}
	return nil
}

// ack records the delivered generation (the pack's, not the local
// fingerprint — the ack states what was delivered).
func (r *Runtime) ack(pack *Pack) {
	if r.cfg.Identity.ServiceID == "" {
		return
	}
	if err := r.cfg.DB.SetConfigAck(wocadb.ConfigAck{
		ServiceID: r.cfg.Identity.ServiceID, Role: r.cfg.Identity.Role,
		Generation: pack.Generation, Addr: r.cfg.Identity.Addr,
		Capabilities: r.cfg.Identity.Capabilities, UpdatedAt: wocadb.Now(),
	}); err != nil {
		r.cfg.log.Warn().Err(err).Msg("nacosrt: config ack write failed")
	}
}

// LastGeneration returns the generation of the last applied pack ("" when
// none was delivered yet) — the worker's and scheduler's capability
// heartbeats rewrite their ack rows with it between pack deliveries, from
// their own goroutines; the atomic load keeps that race-free.
func (r *Runtime) LastGeneration() string {
	if p := r.pack.Load(); p != nil {
		return p.Generation
	}
	return ""
}

// autodetectIP picks the first non-loopback IPv4 address (container
// deployments: the container IP nacos sees).
func autodetectIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address to advertise (set Advertise explicitly)")
}

// ServerFromEnv reads WOCA_NACOS_URL — the single knob a role's assembly
// needs; empty means the entry point forgot the registry and must fail.
func ServerFromEnv() string { return strings.TrimSpace(os.Getenv("WOCA_NACOS_URL")) }
