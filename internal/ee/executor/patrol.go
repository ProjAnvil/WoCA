package executor

// Disk patrol (spec §8.4 defense item 3, the third disk closure): a
// periodic WARN-level watch over the executor's host-side disk posture —
// the workspace root's filesystem watermark and each workspace
// directory's on-disk size. The first version observes and alarms only
// (no enforcement): the archive cap bounds the manual import channel,
// container teardown reclaims the writable layer, and this patrol is the
// explicit boundary that notices everything else — a runaway run writing
// into its bind-mounted /workspace (the mount means container writes hit
// the volume directly), or a host filling up from neighbors.
//
// Costs are bounded but real: each pass statfs's the root and walks
// every workspace directory (first version walks unconditionally; with
// many or huge workspaces this is the knob to revisit — recorded as a
// known trade-off, not hidden).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
)

// patrolInterval is the patrol cadence.
const patrolInterval = 60 * time.Second

// defaultDiskPct is the disk watermark WARN threshold (percent).
const defaultDiskPct = 85.0

// PatrolConfig configures the disk patrol.
type PatrolConfig struct {
	// Store owns the workspace root the patrol watches (required).
	Store *WorkspaceStore
	// Logger receives the WARN alarms. The zero value disables logging.
	Logger zerolog.Logger
	// Interval is the patrol cadence; 0 means 60s.
	Interval time.Duration
	// DiskPct is the filesystem watermark threshold in percent (usage
	// strictly above it alarms); 0 means 85. Adjustable per deployment
	// through WOCA_PATROL_DISK_PCT on the executor entrypoint.
	DiskPct float64
	// DirMaxBytes is the per-workspace-directory size cap; 0 defaults to
	// the store's archive cap (the same budget the manual import channel
	// enforces — a directory persistently past it means in-container
	// writes into the bind mount are outrunning the budget). A negative
	// value disables the per-directory check.
	DirMaxBytes int64
	// DiskUsage overrides the watermark probe (percent used); nil uses
	// the platform statfs. Tests inject deterministic values.
	DiskUsage func(root string) (pct float64, err error)
}

// PatrolLoop runs the patrol periodically until ctx ends. Alarms are
// logs, never panics: the patrol degrades to silence on errors, it does
// not take the executor down.
func PatrolLoop(ctx context.Context, cfg PatrolConfig) {
	if cfg.Interval <= 0 {
		cfg.Interval = patrolInterval
	}
	PatrolOnce(cfg)
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			PatrolOnce(cfg)
		}
	}
}

// PatrolOnce runs one patrol pass: the root filesystem's watermark and
// every workspace directory's size, each against its threshold, WARN on
// breach.
func PatrolOnce(cfg PatrolConfig) {
	if cfg.Store == nil {
		return // nothing rooted to watch
	}
	if cfg.DiskPct <= 0 {
		cfg.DiskPct = defaultDiskPct
	}
	if cfg.DirMaxBytes == 0 && cfg.Store != nil {
		cfg.DirMaxBytes = cfg.Store.maxBytes
	}
	root := ""
	if cfg.Store != nil {
		root = cfg.Store.Root()
	}

	usage := cfg.DiskUsage
	if usage == nil {
		usage = statfsUsage
	}
	if pct, err := usage(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No workspace root yet — no workspace was ever created; not an
			// alarm, and not worth a WARN every minute.
			return
		}
		cfg.Logger.Warn().Err(err).Str("root", root).Msg("patrol: disk watermark probe failed")
	} else if pct > cfg.DiskPct {
		cfg.Logger.Warn().
			Float64("used_pct", pct).
			Float64("threshold_pct", cfg.DiskPct).
			Str("root", root).
			Msg("patrol: workspace-root disk usage over threshold")
	}

	if root == "" || cfg.DirMaxBytes < 0 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			cfg.Logger.Warn().Err(err).Str("root", root).Msg("patrol: workspace root read failed")
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidWorkspaceID(e.Name()) {
			continue // foreign files and non-workspace names are not the patrol's
		}
		dir := filepath.Join(root, e.Name())
		size, err := dirSize(dir)
		if err != nil {
			cfg.Logger.Warn().Err(err).Str("workspace", e.Name()).Msg("patrol: workspace size walk failed")
			continue
		}
		if size > cfg.DirMaxBytes {
			cfg.Logger.Warn().
				Str("workspace", e.Name()).
				Int64("bytes", size).
				Int64("cap", cfg.DirMaxBytes).
				Msg("patrol: workspace directory over size cap")
		}
	}
}

// dirSize sums the regular files under dir (byte sizes; directories and
// symlinks count zero). First version walks the whole tree — see the
// file comment on cost.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("walk %s: %w", dir, err)
	}
	return total, nil
}
