package worker

// Full-stack tenant propagation e2e on a real PostgreSQL (T6): a
// tenant-authenticated run dispatched from the gateway posture is executed
// by a worker and streamed back over ledger SSE to the SAME tenant — the
// exact repro of the bug (pre-fix the worker's events defaulted to the
// 'local' tenant and the tenant's SSE replay came back empty). Skipped
// unless WOCA_PG_TEST_DSN is set; the ledger runs as a NOBYPASSRLS app role
// so the RLS policies actually bind (a superuser connection bypasses them
// and would vacuously pass).

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// fixedTenantAuth authenticates every request as one fixed tenant (the
// gateway-injected identity under test).
type fixedTenantAuth struct{ tenant string }

func (a fixedTenantAuth) Authenticate(context.Context, string) (tenancy.Context, error) {
	return tenancy.Context{TenantID: a.tenant, UserID: "u-e2e"}, nil
}

// openTenantPGDB provisions a throwaway database plus NOBYPASSRLS app role
// and opens wocadb as that role (the migration then runs as the table
// owner, the production posture). Mirrors the runstate/wocadb live-test
// provisioning.
func openTenantPGDB(t *testing.T) (*wocadb.DB, *runstate.Ledger) {
	t.Helper()
	baseDSN := os.Getenv("WOCA_PG_TEST_DSN")
	if baseDSN == "" {
		t.Skip("WOCA_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("woca_e2e_tenant_%d", os.Getpid())
	role, pass := dbName+"_app", "e2e-test"
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`,
		`CREATE DATABASE ` + dbName,
		`DROP ROLE IF EXISTS ` + role,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS`, role, pass),
		`GRANT ALL ON DATABASE ` + dbName + ` TO ` + role,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("provision %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+role)
		_ = admin.Close()
	})

	swap := func(user, pass, name string) string {
		u, err := url.Parse(baseDSN)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, pass)
		u.Path = "/" + name
		return u.String()
	}
	// PG 15+: ordinary roles cannot CREATE on public; grant before Open.
	grant, err := sql.Open("pgx", swap("woca", "woca-enterprise-dev", dbName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.ExecContext(ctx, `GRANT CREATE ON SCHEMA public TO `+role); err != nil {
		_ = grant.Close()
		t.Fatal(err)
	}
	_ = grant.Close()

	db, err := wocadb.Open("pg:" + swap(role, pass, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, ledger
}

// sseEventsBounded follows a run's SSE stream until the terminal frame or
// the deadline — the deadline failure IS the pre-fix symptom (a stream that
// never delivers the worker's events to the tenant reader).
func sseEventsBounded(t *testing.T, rawURL string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v (run events never reached the tenant reader?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", resp.StatusCode)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "event:")
		if !ok {
			continue
		}
		ev := strings.TrimSpace(rest)
		events = append(events, ev)
		if ev == "done" || ev == "error" {
			return events
		}
	}
	t.Fatalf("stream ended without a terminal frame (events so far: %v; ctx err: %v)", events, ctx.Err())
	return nil
}

// TestDispatchTenantEndToEndOnPostgres: tenant POST /run (dispatch form) →
// worker claims and executes → the same tenant's SSE stream replays the
// worker's events to the terminal frame.
func TestDispatchTenantEndToEndOnPostgres(t *testing.T) {
	db, ledger := openTenantPGDB(t)

	// A live sandbox-capable executor ack so the off-host dispatch gate
	// admits the tenant run (design §10.2 M-3), and an enabled sandbox
	// backend signal.
	if err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "exec-e2e", Role: "executor", Generation: "g1",
		Capabilities: "sandbox", UpdatedAt: wocadb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Config{
		Agent: e2eAgent(t, "never-executed-here"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: ledger, DispatchToQueue: true, RunsOffHost: true,
		RunStateDB: db, SandboxEnabled: func() bool { return true },
		Auth: fixedTenantAuth{tenant: "acme"},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{"session_id": "s-tenant-e2e", "prompt": "run for acme"})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out.RunID == "" {
		t.Fatalf("dispatch status = %d runID = %q", resp.StatusCode, out.RunID)
	}

	// The worker shares only the database; its context carries no tenant —
	// the claimed run restores it (both for its event appends and for its
	// session_state marks below).
	w := New(Config{
		Ledger: ledger, Agent: e2eAgent(t, "worker executed for acme"),
		Bus: eventbus.NewLocalWithRetention(16), Logger: zerolog.Nop(), ID: "w-tenant-e2e",
		States: NewDBSessionStates(db),
	})
	go func() { _ = w.Step(t.Context()) }()

	// The session's busy row is tenant-tagged: the gateway's enqueue mark
	// and the worker's claim mark both write under the run's tenant (the
	// table is FORCE RLS-scoped), so the owning tenant's snapshot sees the
	// session as running while the tenantless default does not. Pre-fix both
	// writes landed 'local' and the tenant's listing never showed the busy
	// state. Polled until the terminal clear removes the row.
	acmeCtx := tenancy.NewContext(context.Background(), tenancy.Context{TenantID: "acme"})
	sawTenantRow, sawLocalRow := false, false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := db.SessionStates(acmeCtx)
		if err != nil {
			t.Fatalf("tenant session_state snapshot: %v", err)
		}
		for _, r := range rows {
			if r.SessionID == "s-tenant-e2e" && r.RunID == out.RunID {
				sawTenantRow = true
			}
		}
		localRows, err := db.SessionStates(context.Background())
		if err != nil {
			t.Fatalf("local session_state snapshot: %v", err)
		}
		for _, r := range localRows {
			if r.SessionID == "s-tenant-e2e" {
				sawLocalRow = true
			}
		}
		if sawTenantRow {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawTenantRow {
		t.Fatal("session_state row for the tenant run never appeared in the tenant's snapshot")
	}
	if sawLocalRow {
		t.Fatal("session_state row for the tenant run leaked into the 'local' default view")
	}

	events := sseEventsBounded(t, srv.URL+"/api/runs/"+out.RunID+"/events")
	var delta, done bool
	for _, ev := range events {
		switch ev {
		case "delta":
			delta = true
		case "done":
			done = true
		}
	}
	if !delta || !done {
		t.Fatalf("tenant stream events = delta:%v done:%v (%v)", delta, done, events)
	}
	// The row itself carries the tenant (the queue operator can verify it).
	var tenantID string
	if err := db.QueryRow(`SELECT tenant_id FROM runs WHERE run_id = $1`, out.RunID).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if tenantID != "acme" {
		t.Fatalf("runs.tenant_id = %q, want acme", tenantID)
	}
}
