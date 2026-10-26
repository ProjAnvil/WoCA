package executor

// Labeled leftover collection (design §10.3, spec §8.4's explicit disk
// boundary): every sandbox container this executor identity created
// carries woca.sandbox=1 + woca.executor=<id>; anything so labeled that
// is not in the live service registry is a leftover — a crashed
// predecessor's container, or one whose teardown failed — and is
// force-removed. The same pass sweeps the workspace root's crashed sync
// artifacts: <id>.staging-* extraction dirs and *.prev siblings from an
// interrupted atomic swap (WorkspaceStore.Sync), which only this process
// creates.
//
// The startup pass doubles as the daemon-reachability gate (an
// unreachable docker socket fails the executor's boot rather than
// surfacing mid-run); the periodic pass catches what dies later. Only
// this executor's labeled containers are ever touched — a shared
// daemon's other executors are not our business.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Defaults for CollectorConfig's zero values.
const (
	// gcInterval is the periodic sweep cadence.
	gcInterval = 60 * time.Second
	// gcSweepTimeout bounds one sweep's backend calls.
	gcSweepTimeout = 30 * time.Second
	// stagingFreshFor is how long a staging/prev sibling is assumed to
	// belong to an in-flight Sync and left alone. Sync holds the
	// workspace's DirLock, but the GC shares the root directory and has
	// no view of that lock — the mtime threshold is the race fence.
	stagingFreshFor = time.Hour
)

// CollectorConfig configures the labeled leftover collector.
type CollectorConfig struct {
	// Backend lists and removes the labeled containers (required).
	Backend Backend
	// ExecutorID is this executor's stable identity; only containers
	// labeled woca.executor=<ExecutorID> are collected (required).
	ExecutorID string
	// Store owns the workspace root whose staging/prev crash artifacts
	// the sweep deletes. Nil skips the staging sweep.
	Store *WorkspaceStore
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
	// StagingAge is how long a staging/prev sibling must have been
	// untouched before the sweep may delete it; 0 means stagingFreshFor.
	StagingAge time.Duration
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
	if cfg.StagingAge <= 0 {
		cfg.StagingAge = stagingFreshFor
	}
	return &Collector{cfg: cfg, log: cfg.Logger}
}

// StartupSweep runs both sweeps once, synchronously. A container-sweep
// failure is returned (the executor's boot gate: an unreachable daemon
// is a configuration error, not a degraded mode); staging-sweep failures
// are only logged — a wedged root must not keep the executor from
// serving.
func (c *Collector) StartupSweep(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, c.cfg.SweepTimeout)
	defer cancel()
	if _, err := c.SweepContainers(sctx); err != nil {
		return fmt.Errorf("executor: startup sweep: %w", err)
	}
	if c.cfg.Store != nil {
		if _, err := c.SweepStaging(time.Now()); err != nil {
			c.log.Warn().Err(err).Str("root", c.cfg.Store.Root()).Msg("gc: startup staging sweep failed (the periodic pass retries)")
		}
	}
	return nil
}

// Run drives both sweeps periodically until ctx ends. Errors are logged,
// never fatal — a temporarily unreachable daemon degrades collection,
// not serving.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sctx, cancel := context.WithTimeout(ctx, c.cfg.SweepTimeout)
			if _, err := c.SweepContainers(sctx); err != nil {
				c.log.Warn().Err(err).Msg("gc: container sweep failed (next pass retries)")
			}
			cancel()
			if c.cfg.Store != nil {
				if _, err := c.SweepStaging(now); err != nil {
					c.log.Warn().Err(err).Msg("gc: staging sweep failed (next pass retries)")
				}
			}
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

// SweepStaging deletes crashed sync artifacts under the store root and
// repairs the one crash that would otherwise lose a workspace. The
// artifacts (WorkspaceStore.Sync / replaceDir): <id>.staging-<rand> is
// the extraction dir, <id>.staging-<rand>.prev the old tree moved aside
// for the atomic swap. Three crash states:
//
//   - extraction died: bare staging dirs, workspace intact — delete
//     aged ones (the mtime fence keeps an in-flight Sync's dir).
//   - swap completed but the prev cleanup died: workspace plus a prev
//     sibling, staging gone — the prev is a pure leftover; delete.
//   - swap died between the two renames: prev holds the ONLY copy of
//     the tree, root/<id> is missing — recover by renaming prev back,
//     then clean up. This recovery (not a bare delete) is why the sweep
//     exists per-workspace rather than per-name.
//
// A live swap's mid-window state (fresh staging + prev + missing
// workspace) looks exactly like the third crash, so recovery runs only
// when the staging sibling is aged or absent — a live swap's staging is
// always present and fresh, and the window between the two renames is
// far inside the fence.
func (c *Collector) SweepStaging(now time.Time) (removed int, err error) {
	root := c.cfg.Store.Root()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // nothing synced yet — nothing to sweep
		}
		return 0, fmt.Errorf("gc: read workspace root %s: %w", root, err)
	}

	type artifacts struct {
		staging []string // full names, any age
		prev    []string
		fresh   bool // any staging inside the age fence
	}
	byID := map[string]*artifacts{}
	for _, e := range entries {
		name := e.Name()
		id, ok := stagingWorkspaceID(name)
		if !ok {
			continue
		}
		a := byID[id]
		if a == nil {
			a = &artifacts{}
			byID[id] = a
		}
		if strings.HasSuffix(name, ".prev") {
			a.prev = append(a.prev, name)
			continue
		}
		a.staging = append(a.staging, name)
		info, err := e.Info()
		if err != nil {
			c.log.Warn().Err(err).Str("path", filepath.Join(root, name)).Msg("gc: staging artifact stat failed")
			continue
		}
		if now.Sub(info.ModTime()) < c.cfg.StagingAge {
			a.fresh = true
		}
	}

	for id, a := range byID {
		ws := filepath.Join(root, id)
		_, wsErr := os.Lstat(ws)
		wsExists := wsErr == nil
		switch {
		case len(a.prev) > 0 && !wsExists && a.fresh:
			// The live swap window's shape — leave everything; the next
			// pass decides once the fence has passed.
		case len(a.prev) > 0 && !wsExists:
			// Crash between the renames: one prev holds the tree's only
			// copy — the NEWEST one. Two crashes can stack (a swap whose
			// prev cleanup died, then a later swap crashing mid-window):
			// the older prev is stale residue, and ReadDir's lexical
			// order would happily restore it while deleting the
			// authoritative sibling. A prev dir's mtime tracks its
			// tree's last content change (extraction stamps now), so the
			// newest mtime is the surviving tree; a tie is
			// indeterminable — refuse rather than guess a generation.
			newest, tie := newestPrev(root, a.prev)
			if tie || newest < 0 {
				c.log.Warn().Str("workspace", id).Strs("prev", a.prev).
					Msg("gc: crashed swap recovery ambiguous (no determinable newest prev); left for manual recovery (staged trees were still cleaned)")
				removed += c.removeStaging(root, a.staging, now)
				continue
			}
			if err := os.Rename(filepath.Join(root, a.prev[newest]), ws); err != nil {
				c.log.Warn().Err(err).Str("workspace", id).Msg("gc: crashed swap recovery failed (next pass retries)")
				continue
			}
			c.log.Info().Str("workspace", id).Msg("gc: restored workspace from an interrupted sync's set-aside tree")
			rest := append(a.prev[:newest:newest], a.prev[newest+1:]...)
			removed += c.removeStaging(root, append(a.staging, rest...), now)
		default:
			// Healthy workspace (or none): every artifact is a leftover.
			removed += c.removeStaging(root, append(a.staging, a.prev...), now)
		}
	}
	return removed, nil
}

// removeStaging deletes the named artifacts under root, applying the
// age fence to staging dirs (an in-flight Sync's extraction target) but
// not to prev siblings (a prev next to an existing workspace is always
// post-install residue — its mtime is the old tree's, not its own).
// Returns how many entries were removed.
func (c *Collector) removeStaging(root string, names []string, now time.Time) int {
	removed := 0
	for _, name := range names {
		p := filepath.Join(root, name)
		if !strings.HasSuffix(name, ".prev") {
			info, err := os.Lstat(p)
			if err != nil {
				continue // vanished mid-sweep — fine
			}
			if now.Sub(info.ModTime()) < c.cfg.StagingAge {
				continue // possibly an in-flight Sync's extraction dir
			}
		}
		if err := os.RemoveAll(p); err != nil {
			c.log.Warn().Err(err).Str("path", p).Msg("gc: staging artifact removal failed (next pass retries)")
			continue
		}
		removed++
		c.log.Info().Str("path", p).Msg("gc: removed crashed sync artifact")
	}
	return removed
}

// newestPrev returns the index into prevs of the sibling with the
// newest mtime, and whether the two newest are indistinguishable (a
// tie — the caller must refuse the recovery rather than guess). -1
// (with tie=false) when nothing could be stat'd.
func newestPrev(root string, prevs []string) (best int, tie bool) {
	best = -1
	var bestMod time.Time
	for i, name := range prevs {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			continue // vanished mid-sweep
		}
		switch {
		case info.ModTime().After(bestMod):
			best, bestMod, tie = i, info.ModTime(), false
		case info.ModTime().Equal(bestMod):
			tie = true // matches the current best: the top is ambiguous
		}
	}
	return best, tie
}

// stagingWorkspaceID reports whether name is a Sync artifact under the
// store root — <workspace-id>.staging-<rand> (the extraction dir,
// MkdirTemp's pattern) or <workspace-id>.staging-<rand>.prev (the
// interrupted swap's moved-aside tree) — returning the workspace id it
// belongs to. Only this process mints such names, and the prefix must
// be a valid id, so a workspace or foreign file named to collide is not
// swept.
func stagingWorkspaceID(name string) (id string, ok bool) {
	i := strings.Index(name, ".staging-")
	if i <= 0 {
		return "", false
	}
	if !ValidWorkspaceID(name[:i]) {
		return "", false
	}
	return name[:i], true
}
