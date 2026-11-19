package server

// Tenant propagation through locally executed runs (the non-dispatch
// twin of tenant_dispatch_test.go): a run triggered by a tenant-carrying
// request executes in this process, so its run context — the context every
// ledger write below it uses (Start, the event mirror, session-state marks)
// — must carry the request's tenant. The context's LIVENESS still derives
// from Background (a local run outlives its HTTP request); only the tenant
// value crosses the seam.

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// openPgShapedLedger opens a sqlite database driven as the postgres dialect
// (the wocadb twin of runstate's newPgShapedLedger): tenant-scoped writes
// reach the BeginTx + set_config prologue exactly as they would on a real
// postgres ledger, and fail on the set_config call — the observable that a
// write was tenant-scoped instead of pool-defaulted to 'local'.
func openPgShapedLedger(t *testing.T) *runstate.Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "pgshaped.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pg := &wocadb.DB{DB: db.DB, Dialect: wocadb.DialectPostgres, Path: db.Path}
	l, err := runstate.New(pg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestLocalRunContextCarriesRequestTenant: startRun with a tenant-carrying
// context produces a registered run whose execution context carries the same
// tenant, while the liveness parent stays Background — cancelling the
// triggering request does not cancel the run.
func TestLocalRunContextCarriesRequestTenant(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "local reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: openRunLedger(t),
	})

	origin, cancelOrigin := context.WithCancel(t.Context())
	origin = tenancy.NewContext(origin, tenancy.Context{TenantID: "tenant-a", UserID: "u1"})
	run, _, _, err := s.startRun(origin, "s-local-tenant", messages.Human("run locally"), "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The executed-under context carries the request's tenant (the same
	// extraction helper the ledger's tenant prologue uses).
	tc, ok := tenancy.FromContext(run.ctx)
	if !ok || tc.TenantID != "tenant-a" || tc.UserID != "u1" {
		t.Fatalf("run ctx tenant = %+v ok=%v, want tenant-a/u1", tc, ok)
	}

	// Liveness semantics are preserved: the run derives from Background, so
	// the request ending does not end the run (only run shutdown paths do).
	cancelOrigin()
	select {
	case <-run.ctx.Done():
		t.Fatal("request cancellation must not propagate to a locally executed run")
	default:
	}
}

// TestLocalRunContextWithoutTenant: a tenantless request (the personal
// edition's default) starts a local run with no tenant injected — the ledger
// keeps its 'local' default, unchanged behavior.
func TestLocalRunContextWithoutTenant(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "local reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: openRunLedger(t),
	})

	run, _, _, err := s.startRun(t.Context(), "s-local-plain", messages.Human("run locally"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tenancy.FromContext(run.ctx); ok {
		t.Fatal("local run context must not carry a tenant when the request had none")
	}
}

// TestCommandRunContextCarriesRequestTenant: command runs (/undo, /redo)
// funnel through the same newRunContext, so a tenant-scoped request's
// command run carries the tenant too (its session-state marks below need
// it).
func TestCommandRunContextCarriesRequestTenant(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "local reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-b"})
	run, _, _, err := s.startCommandRun(ctx, "s-cmd-tenant", messages.Human("/undo"), "",
		func(context.Context) string { return "undone" })
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := tenancy.FromContext(run.ctx)
	if !ok || tc.TenantID != "tenant-b" {
		t.Fatalf("command run ctx tenant = %+v ok=%v, want tenant-b", tc, ok)
	}
}

// TestLocalRunLedgerStartReachesTenantPrologue: with a postgres-shaped
// ledger (sqlite driven as postgres dialect), a tenant-carrying local run's
// ledger.Start routes through the tenant-transaction prologue — the
// set_config rejection is the observable that the row write was scoped
// (pre-fix Start saw a tenantless run context and wrote straight to the
// pool, tagging the row 'local').
func TestLocalRunLedgerStartReachesTenantPrologue(t *testing.T) {
	ledger := openPgShapedLedger(t)
	s := New(Config{
		Agent: newScriptedAgent(t, "local reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: ledger,
	})

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-a"})
	run, _, _, err := s.startRun(ctx, "s-prologue", messages.Human("run locally"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for !run.isDone() {
		select {
		case <-deadline:
			t.Fatal("run never reached a terminal state")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The rejected Start must have failed the run (the ledger refused the
	// tenant-scoped insert), not silently continued as a 'local' row.
	if run.finishType() != EventError {
		t.Fatalf("run finish = %q, want error (tenant prologue rejected the Start)", run.finishType())
	}
}
