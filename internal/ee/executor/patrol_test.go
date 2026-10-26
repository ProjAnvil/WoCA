package executor

// Disk patrol tests (patrol.go): a watermark over the threshold and a
// workspace directory past its cap each raise exactly one WARN, and a
// healthy posture stays silent.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// patrolBuf builds a patrol logger capturing JSON events for assertions.
func patrolBuf() (*bytes.Buffer, zerolog.Logger) {
	var buf bytes.Buffer
	return &buf, zerolog.New(&buf).With().Logger()
}

// An over-threshold watermark and an over-cap workspace each raise a
// WARN naming the breach.
func TestPatrolOnceWarnsOnBreach(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)
	if _, err := store.Dir("ws-big"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ws-big", "blob"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	buf, log := patrolBuf()
	PatrolOnce(PatrolConfig{
		Store:   store,
		Logger:  log,
		DiskPct: 85,
		// Tiny per-dir cap: the store cap would need real 512MiB on disk.
		DirMaxBytes: 1024,
		DiskUsage:   func(string) (float64, error) { return 90.5, nil },
	})
	out := buf.String()
	if !strings.Contains(out, "patrol: workspace-root disk usage over threshold") {
		t.Fatalf("no disk watermark alarm: %s", out)
	}
	if !strings.Contains(out, "patrol: workspace directory over size cap") || !strings.Contains(out, "ws-big") {
		t.Fatalf("no workspace size alarm: %s", out)
	}
	if !strings.Contains(out, `"used_pct":90.5`) {
		t.Fatalf("watermark alarm lacks the used figure: %s", out)
	}
}

// A healthy posture (watermark under the threshold, workspaces under
// the cap) logs nothing.
func TestPatrolOnceSilentWhenHealthy(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)
	if _, err := store.Dir("ws-ok"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ws-ok", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A staging artifact: the patrol must not size it (the GC owns it).
	mkdir(t, filepath.Join(root, "ws-ok.staging-1"))

	buf, log := patrolBuf()
	PatrolOnce(PatrolConfig{
		Store:       store,
		Logger:      log,
		DiskPct:     85,
		DirMaxBytes: 1 << 20,
		DiskUsage:   func(string) (float64, error) { return 12, nil },
	})
	if out := buf.String(); out != "" {
		t.Fatalf("healthy posture logged: %s", out)
	}
}

// A DirMaxBytes of zero inherits the store's archive cap — the sync
// boundary's own budget is the patrol's default threshold.
func TestPatrolOnceDefaultsDirCapToStoreCap(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 2048)
	if _, err := store.Dir("ws-huge"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ws-huge", "blob"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	buf, log := patrolBuf()
	PatrolOnce(PatrolConfig{
		Store:     store,
		Logger:    log,
		DiskUsage: func(string) (float64, error) { return 0, nil },
	})
	if !strings.Contains(buf.String(), "patrol: workspace directory over size cap") {
		t.Fatalf("store-cap default did not alarm on a 4096-byte workspace under a 2048-byte cap: %s", buf.String())
	}
}

// A probe failure is a WARN, not a panic — the patrol degrades to
// silence, it never takes the executor down.
func TestPatrolOnceProbeFailureWarns(t *testing.T) {
	buf, log := patrolBuf()
	PatrolOnce(PatrolConfig{
		Store:   NewWorkspaceStore(t.TempDir(), 1<<20),
		Logger:  log,
		DiskPct: 85,
		DiskUsage: func(string) (float64, error) {
			return 0, os.ErrPermission
		},
	})
	if !strings.Contains(buf.String(), "patrol: disk watermark probe failed") {
		t.Fatalf("no probe-failure warning: %s", buf.String())
	}
}

// The platform statfs helper answers for the temp dir (a real
// filesystem) with a sane percentage — the default probe's smoke test.
func TestStatfsUsage(t *testing.T) {
	pct, err := statfsUsage(t.TempDir())
	if err != nil {
		t.Fatalf("statfsUsage: %v", err)
	}
	if pct < 0 || pct > 100 {
		t.Fatalf("statfsUsage = %v, want [0,100]", pct)
	}
}
