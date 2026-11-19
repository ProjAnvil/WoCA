package runstate

// Tenant-propagation tests (T6): a context carrying the originating
// request's tenant must scope the ledger's row writes (Enqueue/Start/Append
// run through the tenant-transaction prologue), and Claim must hand the
// run's tenant back so the worker can restore it. The postgres GUC path is
// proven without a live server via the postgres-dialect-over-sqlite stand-in
// (sqlite has no set_config, so reaching the prologue fails loudly); the
// full RLS round trip lives in pg_live_test.go behind WOCA_PG_TEST_DSN.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

// newPgShapedLedger opens a sqlite database driven as postgres dialect: the
// tenant-transaction prologue fires (BeginTx + set_config) exactly as it
// would on a real postgres ledger, and fails on the set_config call.
func newPgShapedLedger(t *testing.T) *Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + t.TempDir() + "/pgshaped.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Ledger{db: db, dialect: wocadb.DialectPostgres, now: time.Now}
}

func tenantCtx(id string) context.Context {
	return tenancy.NewContext(context.Background(), tenancy.Context{TenantID: id, UserID: "u1"})
}

// TestEnqueueStartApplyTenantScope: with a tenant in the context, Enqueue
// and Start reach the tenant transaction prologue (the set_config rejection
// is the observable — before T6 both wrote straight to the pool and
// succeeded, tagging the row 'local'). Without a tenant, or with the local
// tenant, both take the shared pool and succeed.
func TestEnqueueStartApplyTenantScope(t *testing.T) {
	l := newPgShapedLedger(t)

	if err := l.Enqueue(tenantCtx("acme"), Run{RunID: "r1", SessionID: "s1"}); err == nil ||
		!strings.Contains(err.Error(), "tenant tx scope") {
		t.Fatalf("Enqueue with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}
	if err := l.Start(tenantCtx("acme"), Run{RunID: "r2", SessionID: "s2"}, "w", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "tenant tx scope") {
		t.Fatalf("Start with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}

	// Controls: no tenant and the local tenant keep the pool path.
	if err := l.Enqueue(context.Background(), Run{RunID: "r3", SessionID: "s3"}); err != nil {
		t.Fatalf("Enqueue without tenant: %v", err)
	}
	if err := l.Enqueue(tenantCtx("local"), Run{RunID: "r4", SessionID: "s4"}); err != nil {
		t.Fatalf("Enqueue as local tenant: %v", err)
	}
	if err := l.Start(tenantCtx("local"), Run{RunID: "r5", SessionID: "s5"}, "w", time.Minute); err != nil {
		t.Fatalf("Start as local tenant: %v", err)
	}
}

// TestAppendAppliesTenantScope: Append's insert runs inside the tenant
// transaction when the context carries one — the shape the worker's tenant
// restoration produces.
func TestAppendAppliesTenantScope(t *testing.T) {
	l := newPgShapedLedger(t)
	if err := l.Enqueue(context.Background(), Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(tenantCtx("acme"), "r1", Event{Kind: "delta", Payload: "{}"}); err == nil ||
		!strings.Contains(err.Error(), "tenant tx scope") {
		t.Fatalf("Append with tenant ctx = %v, want the tenant-transaction prologue to fire", err)
	}
	if _, err := l.Append(context.Background(), "r1", Event{Kind: "delta", Payload: "{}"}); err != nil {
		t.Fatalf("Append without tenant: %v", err)
	}
}

// TestClaimProjectionCarriesTenant: the claim projection returns the run's
// tenant on postgres (the worker's restoration source) and an empty literal
// on sqlite (single-tenant by design, no such column).
func TestClaimProjectionCarriesTenant(t *testing.T) {
	pg := &Ledger{dialect: wocadb.DialectPostgres, now: time.Now}
	if q := pg.claimUpdateSQL(); !strings.Contains(q, "tenant_id") {
		t.Errorf("postgres claim projection misses tenant_id: %s", q)
	}
	sqlite := &Ledger{dialect: wocadb.DialectSQLite, now: time.Now}
	if q := sqlite.claimSelectSQL(); !strings.Contains(q, "'' AS tenant_id") {
		t.Errorf("sqlite claim projection misses the tenant literal: %s", q)
	}
}

// TestSqliteClaimReportsNoTenant: on a sqlite ledger a claimed run carries
// no tenant — the personal-edition posture (the enqueue context's tenant is
// irrelevant there; isolation lives one layer up).
func TestSqliteClaimReportsNoTenant(t *testing.T) {
	l := openLedger(t)
	// Even with a tenant in the context, sqlite enqueues on the pool.
	if err := l.Enqueue(tenantCtx("acme"), Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.Claim(context.Background(), "w", time.Minute)
	if err != nil || got == nil {
		t.Fatalf("Claim = %v err=%v", got, err)
	}
	if got.TenantID != "" {
		t.Fatalf("sqlite claim TenantID = %q, want empty", got.TenantID)
	}
}

// TestSessionTenantSqliteGuard: sqlite ledgers are single-tenant by design
// (no tenant column), so SessionTenant never reports one — the wake path
// keeps the bare context and runs stay 'local', the personal-edition
// posture.
func TestSessionTenantSqliteGuard(t *testing.T) {
	l := openLedger(t)
	if err := l.Enqueue(tenantCtx("acme"), Run{RunID: "r1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.SessionTenant(context.Background(), "s1")
	if err != nil || got != "" {
		t.Fatalf("sqlite SessionTenant = %q, %v; want \"\", nil", got, err)
	}
	if got, err := l.SessionTenant(context.Background(), "unknown"); err != nil || got != "" {
		t.Fatalf("sqlite SessionTenant(unknown) = %q, %v; want \"\", nil", got, err)
	}
}
