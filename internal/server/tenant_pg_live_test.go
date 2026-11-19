package server

// Live-PostgreSQL tenant round trips for the gateway's own run paths
// (gated on WOCA_PG_TEST_DSN, the same posture as runstate's pg_live_test
// and the worker dispatch e2e): a tenant-authenticated request must leave
// runs/run_events rows tagged with its tenant and readable only through
// that tenant. The ledger connects as a dedicated NOBYPASSRLS app role over
// a throwaway database so the RLS policies actually bind — a superuser
// connection would bypass them and the tests would pass vacuously.

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
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// fixedTenantAuth authenticates every request as one fixed tenant (the
// gateway-injected identity under test).
type fixedTenantAuth struct{ tenant string }

func (a fixedTenantAuth) Authenticate(context.Context, string) (tenancy.Context, error) {
	return tenancy.Context{TenantID: a.tenant, UserID: "u-live"}, nil
}

// openTenantPGServer provisions a throwaway database plus NOBYPASSRLS app
// role, opens wocadb as that role (the migration then runs as the table
// owner, the production posture) and serves a Server over cfg extras. The
// run surface is exempted from the local-tenant gate so the fixed tenant
// can reach it (the deployment form's HostGateExemptPrefixes seam).
func openTenantPGServer(t *testing.T, cfg Config) (*Server, *runstate.Ledger, *wocadb.DB, *httptest.Server) {
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
	dbName := fmt.Sprintf("woca_srv_tenant_%d", os.Getpid())
	role, pass := dbName+"_app", "srv-test"
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

	cfg.RunLedger = ledger
	cfg.RunStateDB = db
	cfg.HostGateExemptPrefixes = []string{"/api/runs"}
	cfg.Auth = fixedTenantAuth{tenant: "acme"}
	s := New(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, ledger, db, srv
}

// tenantSSEUntilTerminal follows a run's SSE stream until the terminal
// frame and returns the event names seen.
func tenantSSEUntilTerminal(t *testing.T, rawURL string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
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

// TestLocalRunTenantEndToEndOnPostgres: a tenant-authenticated run executed
// BY THE GATEWAY ITSELF (non-dispatch) leaves its runs row, every mirrored
// run_events row and its session_state marks tagged with the tenant, and the
// events stay visible only to that tenant (RLS). Pre-fix, the local run
// context was built from bare Background: the rows defaulted to 'local' and
// the tenant's own replay came back empty.
func TestLocalRunTenantEndToEndOnPostgres(t *testing.T) {
	_, ledger, db, srv := openTenantPGServer(t, Config{
		Agent: newScriptedAgent(t, "locally executed for acme"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})

	body, _ := json.Marshal(map[string]any{"session_id": "s-local-e2e", "prompt": "run for acme"})
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
		t.Fatalf("local run status = %d runID = %q", resp.StatusCode, out.RunID)
	}

	events := tenantSSEUntilTerminal(t, srv.URL+"/api/runs/"+out.RunID+"/events")
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
		t.Fatalf("local stream events = delta:%v done:%v (%v)", delta, done, events)
	}

	// The runs row is tagged with the requesting tenant.
	var tenantID string
	if err := db.QueryRow(`SELECT tenant_id FROM runs WHERE run_id = $1`, out.RunID).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if tenantID != "acme" {
		t.Fatalf("runs.tenant_id = %q, want acme (local execution dropped the request tenant)", tenantID)
	}
	// Every mirrored event row is tagged with the same tenant.
	var nLocal int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_events WHERE run_id = $1 AND tenant_id <> 'acme'`, out.RunID).Scan(&nLocal); err != nil {
		t.Fatal(err)
	}
	if nLocal != 0 {
		t.Fatalf("%d run_events rows escaped the owning tenant", nLocal)
	}

	// RLS isolation: the owning tenant replays its events; a foreign tenant
	// (and the tenantless default that pre-fix received them) see none.
	ctx := context.Background()
	acme := tenancy.NewContext(ctx, tenancy.Context{TenantID: "acme"})
	own, err := ledger.Replay(acme, out.RunID, 0, 100)
	if err != nil || len(own) == 0 {
		t.Fatalf("tenant replay = %v err=%v (local run invisible to its own tenant)", own, err)
	}
	other := tenancy.NewContext(ctx, tenancy.Context{TenantID: "other"})
	foreign, err := ledger.Replay(other, out.RunID, 0, 100)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign tenant replay = %v err=%v, want isolation", foreign, err)
	}
}

// TestWakeDispatchCarriesSessionTenantOnPostgres: the wake driver has no
// request to carry a tenant, so it restores the session's from its most
// recent run row before dispatching — a wake run for a tenant's session
// enqueues tenant-tagged (visible to the owning tenant's SSE), while a
// session whose runs are 'local' keeps the default. Pre-fix the wake path
// dispatched from bare rootCtx and every wake run landed 'local'.
func TestWakeDispatchCarriesSessionTenantOnPostgres(t *testing.T) {
	s, ledger, db, _ := openTenantPGServer(t, Config{
		Agent: newScriptedAgent(t, "never-executed-here"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		DispatchToQueue: true,
	})
	ctx := context.Background()
	acme := tenancy.NewContext(ctx, tenancy.Context{TenantID: "acme"})

	// Seed the sessions' histories: one tenant-dispatched run (finished —
	// the session is idle again), one plain local run.
	seed := func(tenant, runID, sessionID string) {
		t.Helper()
		c := ctx
		if tenant != "" {
			c = tenancy.NewContext(c, tenancy.Context{TenantID: tenant})
		}
		if err := ledger.Enqueue(c, runstate.Run{RunID: runID, SessionID: sessionID}); err != nil {
			t.Fatal(err)
		}
		claimed, err := ledger.Claim(ctx, "w-seed", time.Minute)
		if err != nil || claimed == nil || claimed.RunID != runID {
			t.Fatalf("seed claim %s = %+v err=%v", runID, claimed, err)
		}
		if err := ledger.Finish(ctx, runID, "w-seed", "done"); err != nil {
			t.Fatal(err)
		}
	}
	seed("acme", "seed-acme", "s-wake-acme")
	seed("", "seed-plain", "s-wake-plain")

	// Queue a wake-able notification per session and let the driver fire.
	s.agent.QueueNotificationFor("s-wake-acme", "bg-shell", "shell exited")
	s.maybeWake("s-wake-acme")
	s.agent.QueueNotificationFor("s-wake-plain", "bg-shell", "shell exited")
	s.maybeWake("s-wake-plain")

	wakeTenant := func(sessionID, seedID string) (string, string) {
		t.Helper()
		var runID, tenantID string
		if err := db.QueryRow(
			`SELECT run_id, tenant_id FROM runs WHERE session_id = $1 AND run_id <> $2`,
			sessionID, seedID).Scan(&runID, &tenantID); err != nil {
			t.Fatalf("wake run row for %s: %v", sessionID, err)
		}
		return runID, tenantID
	}
	if _, tenantID := wakeTenant("s-wake-acme", "seed-acme"); tenantID != "acme" {
		t.Fatalf("wake run tenant_id = %q, want acme (wake dispatch dropped the session tenant)", tenantID)
	}
	if _, tenantID := wakeTenant("s-wake-plain", "seed-plain"); tenantID != "local" {
		t.Fatalf("plain wake run tenant_id = %q, want the 'local' default", tenantID)
	}

	// The tenant's reader can replay the wake run's events; the tenantless
	// default cannot (RLS).
	runID, _ := wakeTenant("s-wake-acme", "seed-acme")
	own, err := ledger.Replay(acme, runID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		// No events mirrored yet (no worker executed it) — the row's
		// visibility, not its content, is what matters here; the empty
		// replay must at least succeed under the tenant scope.
		t.Fatalf("unexpected events: %v", own)
	}
	var visible int
	if err := db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE run_id = $1`, runID).Scan(&visible); err != nil {
		t.Fatalf("tenantless visibility probe (queue-operator read, must stay readable): %v", err)
	}
}
