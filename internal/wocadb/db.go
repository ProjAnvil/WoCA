// Package wocadb opens the woca settings database. SQLite (pure Go driver,
// stored under ~/.woca by default) is the primary backend; PostgreSQL is
// available via a postgres:// DSN for server deployments.
package wocadb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/discovery"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver name "pgx"
	_ "modernc.org/sqlite"             // database/sql driver name "sqlite"
)

// Dialect identifies the SQL flavor of an opened database.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// tenantGUCDefault resolves the effective tenant for RLS policies and row
// defaults from the app.tenant_id GUC. The NULLIF is load-bearing: after a
// transaction-scoped SET LOCAL ends, a pooled connection re-reads the GUC
// as the empty string, not NULL — without the guard such connections would
// match tenant ” instead of 'local' and silently mistag written rows.
const tenantGUCDefault = `COALESCE(NULLIF(current_setting('app.tenant_id', true), ''), 'local')`

// DB is an opened woca database with its dialect and human-readable location.
type DB struct {
	*sql.DB
	Dialect Dialect
	Path    string
}

// DefaultDir returns ~/.woca, the root of woca's on-disk state. WOCA_HOME
// overrides the root — multi-role deployments point every container at one
// shared volume so the secrets master key (and any other on-disk state)
// is identical across roles; without it each container would mint its own
// key and secrets written by one role (e.g. the config plane) could not be
// decrypted by another.
func DefaultDir() string {
	if v := os.Getenv("WOCA_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".woca"
	}
	return filepath.Join(home, ".woca")
}

// DefaultPath returns the default sqlite database location (~/.woca/woca.db).
func DefaultPath() string {
	return filepath.Join(DefaultDir(), "woca.db")
}

// Disabled reports whether target selects the "no database" escape hatch.
func Disabled(target string) bool {
	switch strings.TrimSpace(strings.ToLower(target)) {
	case "none", "off":
		return true
	}
	return false
}

// Open opens (and migrates) the woca database. Target resolution:
//
//	""                        → sqlite at ~/.woca/woca.db
//	"none" | "off"            → nil DB (settings disabled)
//	"sqlite"                  → sqlite at the default path
//	"sqlite:/path/to.db"      → sqlite at path
//	"/path/to.db" | "a.db"    → sqlite at path
//	"postgres://…" | "pg:…"   → PostgreSQL via pgx
//	nacos://… | consul://…    → resolved via internal/discovery first
//	                          (VM private deployments, design §12.1)
func Open(target string) (*DB, error) {
	target = strings.TrimSpace(target)
	if isDiscoveryRef(target) {
		resolved, err := discovery.Resolve(context.Background(), target)
		if err != nil {
			return nil, fmt.Errorf("wocadb: resolve %s: %w", redactRef(target), err)
		}
		target = resolved
	}
	if Disabled(target) {
		return nil, nil
	}

	dialect, dsn, path := parseTarget(target)
	var (
		db  *sql.DB
		err error
	)
	switch dialect {
	case DialectPostgres:
		db, err = sql.Open("pgx", dsn)
	default:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("wocadb: create %s: %w", filepath.Dir(path), err)
		}
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	}
	if err != nil {
		return nil, fmt.Errorf("wocadb: open %s: %w", path, err)
	}
	if dialect == DialectSQLite {
		// A single connection avoids SQLITE_BUSY between concurrent writers
		// (checkpoint puts + settings updates) in one process.
		db.SetMaxOpenConns(1)
	}
	out := &DB{DB: db, Dialect: dialect, Path: path}
	if err := out.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return out, nil
}

// isDiscoveryRef reports whether a target references an external registry
// (design §12.1: nacos://… / consul://… address sources).
func isDiscoveryRef(target string) bool {
	return strings.HasPrefix(target, "nacos://") || strings.HasPrefix(target, "consul://")
}

// redactRef keeps registry references (which may embed credentials) out of
// error text: only the scheme and host survive.
func redactRef(target string) string {
	if i := strings.Index(target, "/"); i > 0 {
		if j := strings.Index(target[i+1:], "/"); j >= 0 {
			return target[:i+1+j]
		}
		return target
	}
	return target
}

// parseTarget splits a target into dialect, DSN, and display path.
func parseTarget(target string) (Dialect, string, string) {
	switch {
	case target == "" || strings.EqualFold(target, "sqlite"):
		return DialectSQLite, DefaultPath(), DefaultPath()
	case strings.HasPrefix(target, "sqlite:"):
		path := strings.TrimPrefix(target, "sqlite:")
		return DialectSQLite, path, path
	case strings.HasPrefix(target, "postgres://"), strings.HasPrefix(target, "postgresql://"),
		strings.HasPrefix(target, "pg:"):
		dsn := strings.TrimPrefix(target, "pg:")
		return DialectPostgres, dsn, summarizeDSN(dsn)
	default:
		// A bare path is treated as a sqlite file.
		return DialectSQLite, target, target
	}
}

// summarizeDSN reduces a DSN to scheme://host/db so it can be shown in the UI
// without leaking credentials.
func summarizeDSN(dsn string) string {
	if i := strings.IndexByte(dsn, '@'); i >= 0 && strings.Contains(dsn[:i], "://") {
		if j := strings.Index(dsn, "://"); j >= 0 && j < i {
			return dsn[:j+3] + dsn[i+1:]
		}
	}
	return dsn
}

// migrate creates the woca schema. All tables are keyed simply: settings and
// secrets are single-row-per-key; checkpoints store graph checkpoints;
// memory is a namespace+key JSON document store; session_state records the
// durable per-session run state (running / awaiting an interrupt answer).
//
// On postgres every business table additionally carries tenant_id and is
// RLS-scoped (design doc §9.1, Phase 1). The policy compares against the
// per-transaction GUC app.tenant_id, defaulting to 'local' when unset —
// so existing single-tenant deployments (which never SET it) keep seeing
// and writing exactly their 'local' rows, while tenant-scoped transactions
// (BEGIN; SET LOCAL app.tenant_id = …) see only their own. FORCE ROW LEVEL
// SECURITY makes the table owner subject to the policy too, since the app
// role creates and therefore owns these tables. The sqlite backend stays
// single-tenant by design: no tenant column, isolation is enforced one
// layer up (store namespace prefixes + route gates).
func (d *DB) migrate() error {
	var stmts []string
	switch d.Dialect {
	case DialectPostgres:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secrets (
				name TEXT PRIMARY KEY,
				ciphertext BYTEA NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS checkpoints (
				key TEXT PRIMARY KEY,
				node TEXT NOT NULL,
				"values" TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS memory (
				namespace TEXT NOT NULL,
				key TEXT NOT NULL,
				value TEXT NOT NULL,
				updated_at BIGINT NOT NULL,
				PRIMARY KEY (namespace, key)
			)`,
			`CREATE TABLE IF NOT EXISTS runs (
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
			`CREATE TABLE IF NOT EXISTS run_events (
				run_id TEXT NOT NULL,
				seq BIGINT NOT NULL,
				kind TEXT NOT NULL,
				payload TEXT NOT NULL,
				at BIGINT NOT NULL,
				PRIMARY KEY (run_id, seq)
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS one_active_run_per_session
				ON runs(session_id) WHERE status IN ('queued', 'running')`,
			`CREATE TABLE IF NOT EXISTS session_state (
				session_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				run_id TEXT NOT NULL,
				owner TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS model_presets (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				max_tokens BIGINT NOT NULL DEFAULT 0,
				temperature REAL NOT NULL DEFAULT 0,
				base_url TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS config_acks (
				service_id TEXT PRIMARY KEY,
				role TEXT NOT NULL,
				generation TEXT NOT NULL,
				addr TEXT NOT NULL DEFAULT '',
				updated_at BIGINT NOT NULL
			)`,
		}
		// The lgcp ownership policies (below) judge "which tenant owns this
		// checkpoint thread" by querying runs; that subquery must see rows of
		// every tenant, so runs must not FORCE its policy onto the table
		// owner (the role non-tenant connections run as). Tenant-scoped
		// paths still wrap their own queries in SET LOCAL transactions
		// (store.begin, runstate.begin), so this widens only policy
		// subqueries, not application reads.
		stmts = append(stmts, `ALTER TABLE runs NO FORCE ROW LEVEL SECURITY`)
		for _, table := range []string{"settings", "secrets", "checkpoints", "memory", "session_state", "runs", "run_events"} {
			stmts = append(stmts,
				`ALTER TABLE `+table+` ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT `+tenantGUCDefault,
				// Reset the default every migrate: an earlier Phase 1 build
				// shipped a COALESCE without the NULLIF empty-string guard.
				`ALTER TABLE `+table+` ALTER COLUMN tenant_id SET DEFAULT `+tenantGUCDefault,
				`ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`,
				`ALTER TABLE `+table+` FORCE ROW LEVEL SECURITY`,
				`DO $$
				DECLARE existing text;
				BEGIN
					SELECT qual INTO existing FROM pg_policies
					WHERE tablename = '`+table+`' AND policyname = 'tenant_isolation';
					-- Replace policies from earlier Phase 1 builds (no NULLIF guard).
					IF existing IS NOT NULL AND existing NOT LIKE '%NULLIF%' THEN
						DROP POLICY tenant_isolation ON `+table+`;
						existing := NULL;
					END IF;
					IF existing IS NULL THEN
						CREATE POLICY tenant_isolation ON `+table+`
							USING (tenant_id = `+tenantGUCDefault+`)
							WITH CHECK (tenant_id = `+tenantGUCDefault+`);
					END IF;
				END $$;`)
		}
		// Checkpoint tenant ownership (design §6.3.4 / 2026-08-20 decision): the
		// upstream langgraph/checkpoint/postgres saver's tables are pre-created
		// here in a dedicated schema (they share the name "checkpoints" with
		// woca's own KV table, so schema isolation is mandatory; the saver's
		// search_path points here). RLS derives each row's owner from the run
		// ledger: a thread recorded in runs (server-executed turns, T2) belongs
		// to that run's tenant; threads with no ledger row (subagent and
		// goal-loop internal threads) are admitted — unguessable scratch.
		stmts = append(stmts,
			`CREATE SCHEMA IF NOT EXISTS lgcp`,
			`CREATE TABLE IF NOT EXISTS lgcp.checkpoint_migrations (
			v INTEGER PRIMARY KEY
		)`,
			`CREATE TABLE IF NOT EXISTS lgcp.checkpoints (
			thread_id TEXT NOT NULL,
			checkpoint_ns TEXT NOT NULL DEFAULT '',
			checkpoint_id TEXT NOT NULL,
			parent_checkpoint_id TEXT,
			type TEXT,
			checkpoint JSONB NOT NULL,
			metadata JSONB NOT NULL DEFAULT '{}',
			PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id)
		)`,
			`CREATE TABLE IF NOT EXISTS lgcp.checkpoint_blobs (
			thread_id TEXT NOT NULL,
			checkpoint_ns TEXT NOT NULL DEFAULT '',
			channel TEXT NOT NULL,
			version BIGINT NOT NULL,
			type TEXT NOT NULL,
			blob BYTEA,
			PRIMARY KEY (thread_id, checkpoint_ns, channel, version)
		)`,
			`CREATE TABLE IF NOT EXISTS lgcp.checkpoint_writes (
			thread_id TEXT NOT NULL,
			checkpoint_ns TEXT NOT NULL DEFAULT '',
			checkpoint_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			idx INTEGER NOT NULL,
			channel TEXT NOT NULL,
			type TEXT,
			blob BYTEA NOT NULL,
			PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id, task_id, idx)
		)`,
			`CREATE INDEX IF NOT EXISTS runs_thread_id_idx ON runs(thread_id)`,
		)
		// Ownership lookup: SECURITY DEFINER so the policy sees the true
		// owner of a thread regardless of the caller's own visibility into
		// runs (runs is itself RLS-scoped; a plain subquery would silently
		// see "no owner" for foreign tenants and mis-admit their rows).
		stmts = append(stmts,
			`CREATE OR REPLACE FUNCTION lgcp.thread_owner(thread text) RETURNS text
			LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
			AS $$ SELECT r.tenant_id FROM runs r WHERE r.thread_id = thread AND r.thread_id <> '' LIMIT 1 $$`,
		)
		// Ownership predicate: unowned threads pass; owned threads pass only
		// for their owner's tenant (the same GUC the business tables use).
		const lgcpOwner = `(lgcp.thread_owner(__TBL__.thread_id) IS NULL OR lgcp.thread_owner(__TBL__.thread_id) = ` + tenantGUCDefault + `)`
		for _, table := range []string{"lgcp.checkpoints", "lgcp.checkpoint_blobs", "lgcp.checkpoint_writes"} {
			policy := strings.ReplaceAll(lgcpOwner, "__TBL__", table)
			stmts = append(stmts,
				`ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`,
				`ALTER TABLE `+table+` FORCE ROW LEVEL SECURITY`,
				`DO $$
				DECLARE existing text;
				BEGIN
					SELECT qual INTO existing FROM pg_policies
					WHERE schemaname = 'lgcp' AND tablename = '`+strings.TrimPrefix(table, "lgcp.")+`' AND policyname = 'lgcp_tenant_isolation';
					IF existing IS NULL THEN
						CREATE POLICY lgcp_tenant_isolation ON `+table+`
							USING `+policy+`
							WITH CHECK `+policy+`;
					END IF;
				END $$;`)
		}

	default:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secrets (
				name TEXT PRIMARY KEY,
				ciphertext BLOB NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS checkpoints (
				key TEXT PRIMARY KEY,
				node TEXT NOT NULL,
				"values" BLOB NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS memory (
				namespace TEXT NOT NULL,
				key TEXT NOT NULL,
				value BLOB NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (namespace, key)
			)`,
			`CREATE TABLE IF NOT EXISTS runs (
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
			`CREATE TABLE IF NOT EXISTS run_events (
				run_id TEXT NOT NULL,
				seq BIGINT NOT NULL,
				kind TEXT NOT NULL,
				payload TEXT NOT NULL,
				at BIGINT NOT NULL,
				PRIMARY KEY (run_id, seq)
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS one_active_run_per_session
				ON runs(session_id) WHERE status IN ('queued', 'running')`,
			`CREATE TABLE IF NOT EXISTS session_state (
				session_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				run_id TEXT NOT NULL,
				owner TEXT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS model_presets (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				max_tokens BIGINT NOT NULL DEFAULT 0,
				temperature REAL NOT NULL DEFAULT 0,
				base_url TEXT NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS config_acks (
				service_id TEXT PRIMARY KEY,
				role TEXT NOT NULL,
				generation TEXT NOT NULL,
				addr TEXT NOT NULL DEFAULT '',
				updated_at BIGINT NOT NULL
			)`,
		}
	}
	return d.WithMigrateLock(func(q Querier) error {
		for _, s := range stmts {
			if _, err := q.Exec(s); err != nil {
				return fmt.Errorf("wocadb: migrate: %w", err)
			}
		}
		return nil
	})
}

// migrateLockKey serializes schema migrations across woca processes. PG's
// CREATE TABLE IF NOT EXISTS is not race-safe (concurrent creators collide
// on the pg_type rowtype unique index), and server editions start N
// replicas against one database — the enterprise stack hit exactly that:
// woca-server ×2 + worker migrating simultaneously failed with
// "duplicate key value violates unique constraint pg_type_typname_nsp_index".
const migrateLockKey = 0x776F6361 // "woca"

// Querier is the statement-execution surface shared by *sql.DB and *sql.Tx,
// so migration callbacks run unchanged inside or outside a transaction.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// WithMigrateLock runs fn serialized against every other woca process
// migrating the same database. On Postgres it holds a transaction-scoped
// advisory lock for the whole callback (lock and DDL on one connection);
// sqlite runs the callback inline — its single connection serializes
// writers in-process, and cross-process sqlite is not a supported server
// deployment.
func (d *DB) WithMigrateLock(fn func(q Querier) error) error {
	return WithMigrateLock(d.Dialect, d.DB, fn)
}

// WithMigrateLock is the package form for stores that hold a raw *sql.DB
// plus the dialect (tenant, skillsvc): same advisory-lock serialization.
func WithMigrateLock(dialect Dialect, db *sql.DB, fn func(q Querier) error) error {
	if dialect != DialectPostgres {
		return fn(db)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("wocadb: migrate lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("wocadb: migrate lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("wocadb: migrate lock commit: %w", err)
	}
	return nil
}

// Now returns the current unix timestamp in milliseconds.
func Now() int64 { return time.Now().UnixMilli() }
