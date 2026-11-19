package server

// Tenant for wake-driver runs: the wake paths (notification observer,
// run-finish hook) have no request context, so the session's tenant is
// restored from its run ledger rows (Ledger.SessionTenant) and injected
// into the wake run's context before startRunWith — dispatch mode then
// enqueues a tenant-tagged row and local mode executes under the tenant.
// The sqlite-runnable tests here pin the default posture (no derivable
// tenant → bare rootCtx, the personal edition unchanged); the tenant path
// is proven on live PostgreSQL in tenant_pg_live_test.go.

import (
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// TestWakeRunWithoutDerivableTenantKeepsDefault: on a sqlite ledger (no
// tenant column — SessionTenant always reports none) a wake run starts
// under the bare server lifecycle context: no tenant value is injected, the
// run keeps the 'local' default.
func TestWakeRunWithoutDerivableTenantKeepsDefault(t *testing.T) {
	s, _, _ := newDispatchServer(t, false)
	capture := &ctxCaptureState{}
	s.state = capture

	s.agent.QueueNotificationFor("s-wake-plain", "bg-shell", "shell exited")
	s.maybeWake("s-wake-plain")

	got := capture.waitForSet(t)
	if _, ok := tenancy.FromContext(got); ok {
		t.Fatal("wake run context must not carry a tenant when none is derivable (sqlite/personal posture)")
	}
}
