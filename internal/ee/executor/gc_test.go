package executor

// Labeled leftover collection tests (gc.go): the sweep keeps the active
// set, removes only this executor's leftovers, defers while a create is
// in flight, and sweeps aged staging/prev sync artifacts without
// touching live workspaces or fresh staging dirs.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func labeledSpec(executor string) CreateSpec {
	return CreateSpec{
		Image:  "test:1",
		Labels: map[string]string{LabelSandbox: "1", LabelExecutor: executor},
	}
}

func listedIDs(t *testing.T, be Backend, labels map[string]string) []string {
	t.Helper()
	containers, err := be.List(context.Background(), labels)
	if err != nil {
		t.Fatalf("List(%v): %v", labels, err)
	}
	ids := make([]string, 0, len(containers))
	for _, c := range containers {
		ids = append(ids, c.ID())
	}
	return ids
}

func hasID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// The sweep removes this executor's labeled containers that are not in
// the live set, keeps the live ones, and never touches another
// executor's containers even when they carry the sandbox marker.
func TestCollectorSweepsLeftoversKeepsActive(t *testing.T) {
	be := newBackend(t)
	live := mustCreate(t, be, labeledSpec("e1"))
	dead1 := mustCreate(t, be, labeledSpec("e1"))
	dead2 := mustCreate(t, be, labeledSpec("e1"))
	foreign := mustCreate(t, be, labeledSpec("e2"))

	c := NewCollector(CollectorConfig{
		Backend:    be,
		ExecutorID: "e1",
		Active: func() (map[string]struct{}, bool) {
			return map[string]struct{}{live.ID(): {}}, false
		},
		Logger: zerolog.Nop(),
	})
	removed, err := c.SweepContainers(context.Background())
	if err != nil {
		t.Fatalf("SweepContainers: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	ids := listedIDs(t, be, map[string]string{LabelSandbox: "1"})
	if !hasID(ids, live.ID()) {
		t.Fatalf("live sandbox %s was collected: %v", live.ID(), ids)
	}
	if hasID(ids, dead1.ID()) || hasID(ids, dead2.ID()) {
		t.Fatalf("leftover survived the sweep: %v", ids)
	}
	if !hasID(ids, foreign.ID()) {
		t.Fatalf("another executor's container was collected: %v", ids)
	}
}

// With no live-set hook every labeled container is a leftover — the
// startup posture (an empty registry after a fresh boot).
func TestCollectorSweepsAllWithoutActiveHook(t *testing.T) {
	be := newBackend(t)
	a := mustCreate(t, be, labeledSpec("e1"))
	b := mustCreate(t, be, labeledSpec("e1"))

	c := NewCollector(CollectorConfig{Backend: be, ExecutorID: "e1", Logger: zerolog.Nop()})
	removed, err := c.SweepContainers(context.Background())
	if err != nil {
		t.Fatalf("SweepContainers: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if ids := listedIDs(t, be, map[string]string{LabelSandbox: "1"}); hasID(ids, a.ID()) || hasID(ids, b.ID()) {
		t.Fatalf("sweep left containers behind: %v", ids)
	}
}

// While a create is in flight the sweep defers entirely: a sandbox
// between Backend.Create and its registry insert must never be
// collected, live or not.
func TestCollectorDefersWhileCreating(t *testing.T) {
	be := newBackend(t)
	mid := mustCreate(t, be, labeledSpec("e1"))

	c := NewCollector(CollectorConfig{
		Backend:    be,
		ExecutorID: "e1",
		Active: func() (map[string]struct{}, bool) {
			return nil, true
		},
		Logger: zerolog.Nop(),
	})
	removed, err := c.SweepContainers(context.Background())
	if err != nil {
		t.Fatalf("SweepContainers: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (sweep must defer while creating)", removed)
	}
	if ids := listedIDs(t, be, map[string]string{LabelSandbox: "1"}); !hasID(ids, mid.ID()) {
		t.Fatalf("mid-create sandbox was collected: %v", ids)
	}
}

// A backend listing failure surfaces as the sweep's error — the startup
// gate's fail-closed path.
func TestCollectorSweepErrorPropagates(t *testing.T) {
	be := newBackend(t)
	_ = mustCreate(t, be, labeledSpec("e1"))
	c := NewCollector(CollectorConfig{Backend: be, ExecutorID: "e1", Logger: zerolog.Nop()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a dead context fails the backend's List
	if _, err := c.SweepContainers(ctx); err == nil {
		t.Fatal("SweepContainers with a dead context succeeded")
	}
	if err := c.StartupSweep(ctx); err == nil {
		t.Fatal("StartupSweep with a dead context succeeded (the daemon gate must fail closed)")
	}
}

// The staging sweep covers the three crash states plus the live-swap
// window: bare aged staging dirs are deleted, fresh ones kept; a prev
// beside an existing workspace is a post-install leftover; a prev with
// NO workspace (crash between the swap's renames) is restored, not
// deleted — it holds the tree's only copy; and a fresh staging + prev +
// missing workspace (the live swap window's shape) is left entirely
// alone. Workspaces and foreign names are never touched.
func TestCollectorSweepsAgedStagingArtifacts(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)

	old := time.Now().Add(-2 * time.Hour)
	mkdir(t, filepath.Join(root, "ws1.staging-111")) // extraction crash, aged
	mkdir(t, filepath.Join(root, "ws3.staging-333")) // fresh: in-flight Sync
	// Post-install leftover: workspace present, staging gone, prev aged.
	mkdir(t, filepath.Join(root, "ws5"))
	if err := os.WriteFile(filepath.Join(root, "ws5", "keep"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(root, "ws5.staging-555.prev"))
	mkdir(t, filepath.Join(root, "random-dir")) // foreign name
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ws1.staging-111", "ws5", "ws5.staging-555.prev", "random-dir"} {
		if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.Nop()})
	removed, err := c.SweepStaging(time.Now())
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, gone := range []string{"ws1.staging-111", "ws5.staging-555.prev"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the staging sweep", gone)
		}
	}
	for _, keep := range []string{"ws3.staging-333", "ws5", "random-dir", "loose.txt"} {
		if _, err := os.Lstat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("%s was swept: %v", keep, err)
		}
	}
	// The workspace itself was untouched by its prev's cleanup.
	if b, err := os.ReadFile(filepath.Join(root, "ws5", "keep")); err != nil || string(b) != "v2" {
		t.Fatalf("workspace content disturbed: (%q, %v)", b, err)
	}
}

// A crash between the swap's two renames leaves the old tree parked in
// the .prev sibling and NO workspace directory: the sweep restores the
// tree (renames prev back) and deletes the never-installed staging dir.
// Deleting instead would destroy the workspace's only copy.
func TestCollectorRecoversInterruptedSwap(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)
	old := time.Now().Add(-2 * time.Hour)

	mkdir(t, filepath.Join(root, "ws7.staging-777"))
	mkdir(t, filepath.Join(root, "ws7.staging-777.prev"))
	if err := os.WriteFile(filepath.Join(root, "ws7.staging-777.prev", "tree"), []byte("old-copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ws7.staging-777", "ws7.staging-777.prev"} {
		if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.Nop()})
	if _, err := c.SweepStaging(time.Now()); err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	// The workspace is back, holding the set-aside tree.
	b, err := os.ReadFile(filepath.Join(root, "ws7", "tree"))
	if err != nil || string(b) != "old-copy" {
		t.Fatalf("workspace not restored from prev: (%q, %v)", b, err)
	}
	for _, gone := range []string{"ws7.staging-777", "ws7.staging-777.prev"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the recovery sweep", gone)
		}
	}
}

// The double-crash: an older swap whose prev cleanup died, then a newer
// swap crashing between its renames. The lexically-first prev is the
// STALE one; recovery must restore the newest-mtime prev (the tree the
// workspace actually had) and clean the older sibling, never the other
// way around.
func TestCollectorRecoversNewestPrev(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)

	stale := filepath.Join(root, "ws9.staging-001.prev")  // the older crash's residue
	latest := filepath.Join(root, "ws9.staging-002.prev") // the interrupted swap's set-aside tree
	staging := filepath.Join(root, "ws9.staging-003")     // the newer swap's never-installed tree
	for _, p := range []string{stale, latest, staging} {
		mkdir(t, p)
	}
	if err := os.WriteFile(filepath.Join(stale, "gen"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(latest, "gen"), []byte("latest"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAge(t, stale, -3*time.Hour)
	setAge(t, latest, -2*time.Hour)
	setAge(t, staging, -2*time.Hour)

	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.Nop()})
	if _, err := c.SweepStaging(time.Now()); err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "ws9", "gen"))
	if err != nil || string(b) != "latest" {
		t.Fatalf("recovered the wrong generation: content = (%q, %v), want %q", b, err, "latest")
	}
	for _, gone := range []string{stale, latest, staging} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived the recovery sweep", gone)
		}
	}
}

// Two prev siblings with indistinguishable mtimes cannot be ordered:
// the sweep refuses the recovery (WARNs for manual intervention) rather
// than guessing a generation. Staged trees are still cleaned — they are
// never-installed residue regardless.
func TestCollectorRefusesAmbiguousRecovery(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)

	a := filepath.Join(root, "wsA.staging-001.prev")
	b := filepath.Join(root, "wsA.staging-002.prev")
	staging := filepath.Join(root, "wsA.staging-003")
	for _, p := range []string{a, b, staging} {
		mkdir(t, p)
	}
	same := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{a, b, staging} {
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.New(&buf)})
	removed, err := c.SweepStaging(time.Now())
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if removed != 1 { // only the never-installed staged tree
		t.Fatalf("removed = %d, want 1 (only the staging dir)", removed)
	}
	if _, err := os.Lstat(filepath.Join(root, "wsA")); !os.IsNotExist(err) {
		t.Fatal("ambiguous recovery created a workspace anyway")
	}
	for _, keep := range []string{a, b} {
		if _, err := os.Lstat(keep); err != nil {
			t.Fatalf("ambiguous prev %s was swept", keep)
		}
	}
	if !strings.Contains(buf.String(), "recovery ambiguous") {
		t.Fatalf("no ambiguity warning logged: %s", buf.String())
	}
}

// The live swap window — fresh staging + prev + no workspace — has the
// same shape as the crash-between state, so the sweep defers until the
// age fence distinguishes them.
func TestCollectorDefersOnLiveSwapWindow(t *testing.T) {
	root := t.TempDir()
	store := NewWorkspaceStore(root, 1<<20)
	mkdir(t, filepath.Join(root, "ws8.staging-888")) // fresh
	mkdir(t, filepath.Join(root, "ws8.staging-888.prev"))

	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.Nop()})
	removed, err := c.SweepStaging(time.Now())
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (live swap window must be left alone)", removed)
	}
	for _, keep := range []string{"ws8.staging-888", "ws8.staging-888.prev"} {
		if _, err := os.Lstat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("%s was swept in the live window: %v", keep, err)
		}
	}
}

// A missing workspace root (nothing ever synced) is not an error.
func TestCollectorStagingSweepMissingRoot(t *testing.T) {
	store := NewWorkspaceStore(filepath.Join(t.TempDir(), "nope"), 1<<20)
	c := NewCollector(CollectorConfig{Store: store, Logger: zerolog.Nop()})
	if removed, err := c.SweepStaging(time.Now()); err != nil || removed != 0 {
		t.Fatalf("SweepStaging on a missing root = (%d, %v), want (0, nil)", removed, err)
	}
}

// stagingWorkspaceID pins the name contract: MkdirTemp's
// <valid-id>.staging-<rand> plus the .prev sibling, nothing else.
func TestStagingWorkspaceID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"ws1.staging-123456", "ws1", true},
		{"ws-2_x.staging-9", "ws-2_x", true},
		{"ws1.staging-123456.prev", "ws1", true},
		{"ws1.staging-1.prev.prev", "ws1", true}, // still the minted prefix
		{"ws1.staging-", "ws1", true},            // degenerate but mint-shaped
		{"ws1", "", false},                       // a workspace
		{"ws1.staging", "", false},               // no MkdirTemp suffix
		{"..staging-1", "", false},               // empty/invalid id prefix
		{"w.s1staging-1", "", false},             // separator before the marker
		{"staging-1", "", false},                 // no id prefix
		{"ws 1.staging-1", "", false},            // space is not a workspace-id rune
	}
	for _, tt := range tests {
		id, ok := stagingWorkspaceID(tt.name)
		if ok != tt.ok || (ok && id != tt.id) {
			t.Errorf("stagingWorkspaceID(%q) = (%q, %v), want (%q, %v)", tt.name, id, ok, tt.id, tt.ok)
		}
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// setAge stamps a path's times to now+delta (the sweep's age fence and
// the newest-prev ordering both read mtimes).
func setAge(t *testing.T, path string, delta time.Duration) {
	t.Helper()
	when := time.Now().Add(delta)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
