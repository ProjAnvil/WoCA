package server

// Tenant propagation through queue-dispatched runs (T6): the run context a
// dispatched run executes under derives from the server lifecycle (rootCtx)
// but must carry the originating request's tenant identity, so the ledger
// row is tagged with that tenant and the worker restores it from the claimed
// row. The enqueue-site half is proven here; the worker half lives in
// internal/ee/worker, the RLS round trip in runstate's pg-live test.

import (
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
)

// TestDispatchRunContextCarriesRequestTenant: enqueueing with a
// tenant-carrying context produces a queued ledger row whose run context
// carries the same tenant, while keeping the rootCtx liveness parent
// (server shutdown still cancels the dispatched run).
func TestDispatchRunContextCarriesRequestTenant(t *testing.T) {
	s, ledger, _ := newDispatchServer(t, false)

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-a", UserID: "u1"})
	run, _, _, err := s.enqueueRun(ctx, "s-tenant", messages.Human("queue me"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := ledger.Row(t.Context(), run.ID())
	if err != nil || !ok || row.Status != runstate.StatusQueued {
		t.Fatalf("ledger row = %+v ok=%v err=%v", row, ok, err)
	}

	// The executed-under context carries the request's tenant (the same
	// extraction helper the ledger's tenant prologue uses).
	tc, ok := tenancy.FromContext(run.ctx)
	if !ok || tc.TenantID != "tenant-a" || tc.UserID != "u1" {
		t.Fatalf("run ctx tenant = %+v ok=%v, want tenant-a/u1", tc, ok)
	}

	// Liveness semantics are preserved: rootCtx cancellation (server
	// shutdown) still ends the dispatched run's context.
	s.rootCancel()
	select {
	case <-run.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("rootCtx cancellation did not propagate to the dispatched run context")
	}
}

// TestDispatchRunContextWithoutTenant: a tenantless request (local,
// unauthenticated forms) enqueues without injecting any tenant value — the
// ledger then keeps its 'local' default, the personal-edition posture.
func TestDispatchRunContextWithoutTenant(t *testing.T) {
	s, _, _ := newDispatchServer(t, false)

	run, _, _, err := s.enqueueRun(t.Context(), "s-plain", messages.Human("queue me"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tenancy.FromContext(run.ctx); ok {
		t.Fatal("dispatched run context must not carry a tenant when the request had none")
	}
}
