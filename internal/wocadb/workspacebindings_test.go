package wocadb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// seedLegacySQLite stands in for a personal-edition database created by a
// pre-sandbox-plane build: runs without workspace_id, config_acks without
// load, and no workspace_bindings table. The upgrade path (ALTER under
// PRAGMA pre-check) must bring it to the current schema without losing the
// legacy rows.
func seedLegacySQLite(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for _, s := range []string{
		`CREATE TABLE runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			status TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			lease_expires_at BIGINT NOT NULL DEFAULT 0,
			thread_id TEXT NOT NULL DEFAULT '',
			config_snapshot TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)`,
		`CREATE TABLE config_acks (
			service_id TEXT PRIMARY KEY,
			role TEXT NOT NULL,
			generation TEXT NOT NULL,
			addr TEXT NOT NULL DEFAULT '',
			capabilities TEXT NOT NULL DEFAULT '',
			updated_at BIGINT NOT NULL
		)`,
		`INSERT INTO runs (run_id, session_id, status, created_at, updated_at)
			VALUES ('legacy-run', 'legacy-session', 'finished', 1, 1)`,
		`INSERT INTO config_acks (service_id, role, generation, updated_at)
			VALUES ('legacy-svc', 'worker', 'g0', 1)`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed legacy db: %v", err)
		}
	}
}

// openNamedTestDB opens a fresh sqlite database under a distinct name (the
// package's shared openTestDB always uses test.db).
func openNamedTestDB(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open("sqlite:" + filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insertRun inserts one bare runs row (distinct session per row so the
// pre-existing one_active_run_per_session partial index never fires).
func insertRun(t *testing.T, db *DB, runID, sessionID, status, workspaceID string) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO runs (run_id, session_id, status, workspace_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, runID, sessionID, status, workspaceID, Now(), Now())
	return err
}

// TestWorkspaceBindingMigrationIdempotent: opening a fresh database twice
// runs the migration twice over the same file without error, and both the
// new table and the workspace-mutex partial index survive.
func TestWorkspaceBindingMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	for i := 1; i <= 2; i++ {
		db, err := Open("sqlite:" + path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		for _, want := range []string{
			`SELECT name FROM sqlite_master WHERE type='table' AND name='workspace_bindings'`,
			`SELECT name FROM sqlite_master WHERE type='index' AND name='one_running_run_per_workspace'`,
		} {
			var name string
			if err := db.QueryRow(want).Scan(&name); err != nil {
				t.Fatalf("open #%d: %v", i, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWorkspaceBindingMigrationLegacySQLite: a database created by an older
// build upgrades in place — the new columns appear with their defaults
// backfilled onto legacy rows, and the new table is immediately usable.
func TestWorkspaceBindingMigrationLegacySQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacySQLite(t, path)

	db, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var ws string
	if err := db.QueryRow(`SELECT workspace_id FROM runs WHERE run_id = 'legacy-run'`).Scan(&ws); err != nil {
		t.Fatalf("runs.workspace_id missing after upgrade: %v", err)
	}
	if ws != "" {
		t.Fatalf("legacy runs.workspace_id = %q, want '' (column default)", ws)
	}
	var load int64
	if err := db.QueryRow(`SELECT load FROM config_acks WHERE service_id = 'legacy-svc'`).Scan(&load); err != nil {
		t.Fatalf("config_acks.load missing after upgrade: %v", err)
	}
	if load != 0 {
		t.Fatalf("legacy config_acks.load = %d, want 0 (column default)", load)
	}

	got, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_legacy",
		SourceKey:   "t1|/home/u/proj",
		TenantID:    "t1",
		ExecutorID:  "exec-1",
	})
	if err != nil {
		t.Fatalf("ensure on upgraded db: %v", err)
	}
	if got.State != WorkspaceBindingActive || got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Fatalf("ensure on upgraded db = %+v", got)
	}
}

// TestWorkspaceBindingEnsureIdempotent: allocate-or-bind — the second call
// with the same source_key (even proposing a different workspace ID and
// executor) returns the original row verbatim; a fresh source_key allocates
// a new one.
func TestWorkspaceBindingEnsureIdempotent(t *testing.T) {
	db := openNamedTestDB(t, "bindings.db")

	first, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_aaa",
		SourceKey:   "t1|/home/u/proj",
		TenantID:    "t1",
		ExecutorID:  "exec-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != WorkspaceBindingActive {
		t.Fatalf("first state = %q, want %q", first.State, WorkspaceBindingActive)
	}
	if first.CreatedAt == 0 || first.UpdatedAt == 0 {
		t.Fatalf("first ensure did not stamp timestamps: %+v", first)
	}

	second, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_bbb",
		SourceKey:   "t1|/home/u/proj",
		TenantID:    "t1",
		ExecutorID:  "exec-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkspaceID != first.WorkspaceID || second.ExecutorID != "exec-1" ||
		second.CreatedAt != first.CreatedAt || second.UpdatedAt != first.UpdatedAt {
		t.Fatalf("second ensure = %+v, want the original row %+v", second, first)
	}

	other, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_ccc",
		SourceKey:   "t2|/home/u/proj", // same path, other tenant → distinct key
		TenantID:    "t2",
		ExecutorID:  "exec-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.WorkspaceID != "ws_ccc" {
		t.Fatalf("distinct source_key allocated %+v, want ws_ccc", other)
	}
}

// TestWorkspaceBindingLookups: both lookup keys, hits and misses.
func TestWorkspaceBindingLookups(t *testing.T) {
	db := openNamedTestDB(t, "lookups.db")

	if _, ok, err := db.WorkspaceBindingBySourceKey("missing"); err != nil || ok {
		t.Fatalf("by source_key miss = %v, %v; want false, nil", ok, err)
	}
	if _, ok, err := db.WorkspaceBindingByID("missing"); err != nil || ok {
		t.Fatalf("by id miss = %v, %v; want false, nil", ok, err)
	}

	created, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_x",
		SourceKey:   "t1|/home/u/x",
		TenantID:    "t1",
		ExecutorID:  "exec-9",
		CreatedAt:   42,
		UpdatedAt:   42,
	})
	if err != nil {
		t.Fatal(err)
	}

	byKey, ok, err := db.WorkspaceBindingBySourceKey("t1|/home/u/x")
	if err != nil || !ok {
		t.Fatalf("by source_key hit = %v, %v", ok, err)
	}
	if byKey != created {
		t.Fatalf("by source_key = %+v, want %+v", byKey, created)
	}

	byID, ok, err := db.WorkspaceBindingByID("ws_x")
	if err != nil || !ok {
		t.Fatalf("by id hit = %v, %v", ok, err)
	}
	if byID != created {
		t.Fatalf("by id = %+v, want %+v", byID, created)
	}
}

// TestWorkspaceBindingSetState: state transitions bump updated_at and leave
// the rest of the row alone; an unknown id is a no-op, not an error.
func TestWorkspaceBindingSetState(t *testing.T) {
	db := openNamedTestDB(t, "state.db")

	created, err := db.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_s",
		SourceKey:   "t1|/home/u/s",
		TenantID:    "t1",
		ExecutorID:  "exec-1",
		CreatedAt:   42,
		UpdatedAt:   42,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetWorkspaceBindingState("ws_s", "detached"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.WorkspaceBindingByID("ws_s")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got.State != "detached" {
		t.Fatalf("state = %q, want detached", got.State)
	}
	if got.UpdatedAt <= created.UpdatedAt {
		t.Fatalf("updated_at = %d, want > %d", got.UpdatedAt, created.UpdatedAt)
	}
	if got.CreatedAt != created.CreatedAt || got.ExecutorID != created.ExecutorID {
		t.Fatalf("row mutated beyond state/updated_at: %+v", got)
	}

	if err := db.SetWorkspaceBindingState("ws_nope", "detached"); err != nil {
		t.Fatalf("set state on unknown id = %v, want nil (idempotent no-op)", err)
	}
}

// TestWorkspaceBindingsList: every binding, in a deterministic order.
func TestWorkspaceBindingsList(t *testing.T) {
	db := openNamedTestDB(t, "list.db")
	if got, err := db.WorkspaceBindings(); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v", got, err)
	}

	for _, b := range []WorkspaceBinding{
		{WorkspaceID: "ws_2", SourceKey: "t1|/b", TenantID: "t1", ExecutorID: "e1"},
		{WorkspaceID: "ws_1", SourceKey: "t1|/a", TenantID: "t1", ExecutorID: "e1"},
	} {
		if _, err := db.EnsureWorkspaceBinding(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.WorkspaceBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("list = %d rows, want 2", len(got))
	}
	if got[0].SourceKey != "t1|/a" || got[1].SourceKey != "t1|/b" {
		t.Fatalf("list order = [%s, %s], want source_key ascending", got[0].SourceKey, got[1].SourceKey)
	}
}

// TestWorkspaceBindingRunsMutexIndex: the partial unique index on
// runs(workspace_id) enforces one running run per non-empty workspace:
// a second running row for the same workspace is rejected, empty
// workspace_ids never collide, and finishing the run releases the mutex.
func TestWorkspaceBindingRunsMutexIndex(t *testing.T) {
	db := openNamedTestDB(t, "mutex.db")

	if err := insertRun(t, db, "r1", "s1", "running", "ws_a"); err != nil {
		t.Fatal(err)
	}
	if err := insertRun(t, db, "r2", "s2", "running", "ws_a"); err == nil {
		t.Fatal("second running run for ws_a must violate the partial unique index")
	}
	// A different workspace is unaffected.
	if err := insertRun(t, db, "r3", "s3", "running", "ws_b"); err != nil {
		t.Fatal(err)
	}
	// Empty workspace_id rows are excluded by the predicate — legacy runs
	// without a workspace never collide with each other or with ws_a.
	if err := insertRun(t, db, "r4", "s4", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := insertRun(t, db, "r5", "s5", "running", ""); err != nil {
		t.Fatal(err)
	}
	// Non-running statuses never collide either.
	if err := insertRun(t, db, "r6", "s6", "queued", "ws_a"); err != nil {
		t.Fatal(err)
	}

	// Finish r1 (and clear the queued r6 so ws_a is fully idle), then a
	// new running run for ws_a is admissible again.
	for _, s := range []string{
		`UPDATE runs SET status = 'finished' WHERE run_id = 'r1'`,
		`UPDATE runs SET status = 'cancelled' WHERE run_id = 'r6'`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertRun(t, db, "r7", "s7", "running", "ws_a"); err != nil {
		t.Fatalf("run for ws_a after release: %v", err)
	}
}

// TestConfigAckLoadRoundTrip: the heartbeat row carries the executor's
// self-reported load, and a rewrite without a load resets it to zero.
func TestConfigAckLoadRoundTrip(t *testing.T) {
	db := openNamedTestDB(t, "acks-load.db")

	if err := db.SetConfigAck(ConfigAck{
		ServiceID: "exec-1", Role: "worker", Generation: "g1",
		Capabilities: "sandbox", Load: 3, UpdatedAt: Now(),
	}); err != nil {
		t.Fatal(err)
	}
	acks, err := db.ConfigAcks()
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || acks[0].Load != 3 {
		t.Fatalf("acks = %+v, want one with load 3", acks)
	}

	if err := db.SetConfigAck(ConfigAck{
		ServiceID: "exec-1", Role: "worker", Generation: "g2",
		Capabilities: "sandbox", UpdatedAt: Now(),
	}); err != nil {
		t.Fatal(err)
	}
	acks, err = db.ConfigAcks()
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || acks[0].Load != 0 || acks[0].Generation != "g2" {
		t.Fatalf("rewritten ack = %+v, want load 0 generation g2", acks[0])
	}
}

// TestWorkspaceBindingPostgresDialectPaths exercises the $N placeholder
// branches on a deterministically failed connection (the repo's closed-DB
// masquerade pattern from TestSessionStatePostgresDialectPaths).
func TestWorkspaceBindingPostgresDialectPaths(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "pgpaths.db"))
	if err != nil {
		t.Fatal(err)
	}
	pg := &DB{DB: db.DB, Dialect: DialectPostgres, Path: db.Path}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := pg.EnsureWorkspaceBinding(WorkspaceBinding{
		WorkspaceID: "ws_p", SourceKey: "t|/p", TenantID: "t", ExecutorID: "e",
	}); err == nil {
		t.Fatal("EnsureWorkspaceBinding on closed DB should fail")
	}
	if _, _, err := pg.WorkspaceBindingBySourceKey("t|/p"); err == nil {
		t.Fatal("WorkspaceBindingBySourceKey on closed DB should fail")
	}
	if _, _, err := pg.WorkspaceBindingByID("ws_p"); err == nil {
		t.Fatal("WorkspaceBindingByID on closed DB should fail")
	}
	if err := pg.SetWorkspaceBindingState("ws_p", "detached"); err == nil {
		t.Fatal("SetWorkspaceBindingState on closed DB should fail")
	}
	if _, err := pg.WorkspaceBindings(); err == nil {
		t.Fatal("WorkspaceBindings on closed DB should fail")
	}
}
