package executor

// Labeled leftover collection (design §10.3, spec §8.4's explicit disk
// boundary): every sandbox container this executor identity created
// carries woca.sandbox=1 + woca.executor=<id>; anything so labeled that
// is not in the live service registry is a leftover — a crashed
// predecessor's container, or one whose teardown failed — and is
// force-removed.
//
// The startup pass doubles as the daemon-reachability gate (an
// unreachable docker socket fails the executor's boot rather than
// surfacing mid-run); the periodic pass catches what dies later. Only
// this executor's labeled containers are ever touched — a shared
// daemon's other executors are not our business.

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
)

// Defaults for CollectorConfig's zero values.
const (
	// gcInterval is the periodic sweep cadence.
	gcInterval = 60 * time.Second
	// gcSweepTimeout bounds one sweep's backend calls.
	gcSweepTimeout = 30 * time.Second
)

// CollectorConfig configures the labeled leftover collector.
type CollectorConfig struct {
	// Backend lists and removes the labeled containers (required).
	Backend Backend
	// ExecutorID is this executor's stable identity; only containers
	// labeled woca.executor=<ExecutorID> are collected (required).
	ExecutorID string
	// Active snapshots the live set: the ids of every sandbox the
	// service owns, and whether a create is currently in flight (the
	// whole container sweep defers while one is, so a sandbox between
	// Backend.Create and its registry insert is never collected). Nil
	// means "nothing is live" — every labeled container is a leftover.
	Active func() (live map[string]struct{}, creating bool)
	// Logger receives sweep lifecycle logs. The zero value disables
	// logging.
	Logger zerolog.Logger
	// Interval is the periodic pass cadence; 0 means gcInterval.
	Interval time.Duration
	// SweepTimeout bounds each pass's backend calls; 0 means
	// gcSweepTimeout.
	SweepTimeout time.Duration
}

// Collector removes this executor's labeled leftovers, once at startup
// and periodically thereafter.
type Collector struct {
	cfg CollectorConfig
	log zerolog.Logger
}

// NewCollector normalizes the config's zero values.
func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.Interval <= 0 {
		cfg.Interval = gcInterval
	}
	if cfg.SweepTimeout <= 0 {
		cfg.SweepTimeout = gcSweepTimeout
	}
	return &Collector{cfg: cfg, log: cfg.Logger}
}

// StartupSweep runs the sweep once, synchronously. A failure is returned:
// the executor's boot gate — an unreachable daemon is a configuration
// error, not a degraded mode.
func (c *Collector) StartupSweep(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, c.cfg.SweepTimeout)
	defer cancel()
	if _, err := c.SweepContainers(sctx); err != nil {
		return fmt.Errorf("executor: startup sweep: %w", err)
	}
	return nil
}

// Run drives the sweep periodically until ctx ends. Errors are logged,
// never fatal — a temporarily unreachable daemon degrades collection,
// not serving.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, c.cfg.SweepTimeout)
			if _, err := c.SweepContainers(sctx); err != nil {
				c.log.Warn().Err(err).Msg("gc: container sweep failed (next pass retries)")
			}
			cancel()
		}
	}
}

// SweepContainers lists this executor's labeled sandbox containers and
// force-removes every one absent from the live set. The pass defers
// entirely while a create is in flight: the active snapshot is read
// after the listing, so a container that appeared in it is either
// already live (skipped) or mid-provision (creating; the next pass
// collects it if its create died).
func (c *Collector) SweepContainers(ctx context.Context) (removed int, err error) {
	leftovers, err := c.cfg.Backend.List(ctx, map[string]string{
		LabelSandbox:  "1",
		LabelExecutor: c.cfg.ExecutorID,
	})
	if err != nil {
		return 0, fmt.Errorf("list labeled sandboxes: %w", err)
	}
	var live map[string]struct{}
	if c.cfg.Active != nil {
		var creating bool
		live, creating = c.cfg.Active()
		if creating {
			c.log.Debug().Msg("gc: create in flight; container sweep deferred to the next pass")
			return 0, nil
		}
	}
	for _, ctr := range leftovers {
		if _, ok := live[ctr.ID()]; ok {
			continue
		}
		if err := ctr.Remove(ctx, true); err != nil {
			if isRemovedErr(err) {
				continue // already gone — the sweep's goal state
			}
			c.log.Warn().Err(err).Str("sandbox", ctr.ID()).Msg("gc: leftover removal failed (next pass retries)")
			continue
		}
		removed++
	}
	if removed > 0 {
		c.log.Info().Int("count", removed).Msg("gc: removed leftover sandbox containers")
	}
	return removed, nil
}
