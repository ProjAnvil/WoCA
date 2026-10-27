package executor

// Labeled leftover collection tests (gc.go): the sweep keeps the active
// set, removes only this executor's leftovers, and defers while a create
// is in flight. The staging/prev sync-artifact sweep is gone with the
// transfer machinery it cleaned up after (spec §5.3).

import (
	"context"
	"testing"

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
