// Worker-side sandbox backend reads: the materialized table is the
// authority (design §10.3 m-4 — the enabled row is read from the
// materialized table, not the settings overlay).
package nacosrt

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/wocadb"
)

// ensureSandboxBackends provisions the sandbox_backends table on the
// worker side (idempotent; the config plane runs the same DDL — neither
// role depends on the other's boot order, design §10.3 m-3).
func ensureSandboxBackends(db *wocadb.DB) error {
	// Timestamp columns hold unix milliseconds — BIGINT on postgres.
	tsCol := "INTEGER"
	if db.Dialect == wocadb.DialectPostgres {
		tsCol = "BIGINT"
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sandbox_backends (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT 'contract-http',
		endpoint TEXT NOT NULL,
		timeout_ms INTEGER NOT NULL DEFAULT 120000,
		enabled INTEGER NOT NULL DEFAULT 0,
		last_probe_at ` + tsCol + `,
		last_probe_ok INTEGER,
		last_probe_error TEXT,
		options TEXT NOT NULL DEFAULT '',
		created_at ` + tsCol + ` NOT NULL,
		updated_at ` + tsCol + ` NOT NULL
	)`); err != nil {
		return fmt.Errorf("nacosrt: sandbox backends migrate: %w", err)
	}
	// options (design §17 sandbox params) arrived after the first v2
	// deployments: upgrade pre-options tables in place. PG's ADD COLUMN
	// IF NOT EXISTS is idempotent; sqlite has no IF NOT EXISTS for
	// columns, so the duplicate-column error is the marker.
	if db.Dialect == wocadb.DialectPostgres {
		if _, err := db.Exec(`ALTER TABLE sandbox_backends ADD COLUMN IF NOT EXISTS options TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("nacosrt: sandbox backends migrate: %w", err)
		}
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE sandbox_backends ADD COLUMN options TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("nacosrt: sandbox backends migrate: %w", err)
	}
	return nil
}

// EnabledSandbox returns the enabled backend row (options included — the
// executor's parameters), or nil when none.
func EnabledSandbox(db *sql.DB) (*SandboxRow, error) {
	var r SandboxRow
	var enabled int64
	var probeAt, probeOK sql.NullInt64
	var probeErr sql.NullString
	var optsText string
	err := db.QueryRow(`SELECT id, name, type, endpoint, timeout_ms, enabled,
		last_probe_at, last_probe_ok, last_probe_error, options FROM sandbox_backends WHERE enabled = 1`).
		Scan(&r.ID, &r.Name, &r.Type, &r.Endpoint, &r.TimeoutMs, &enabled,
			&probeAt, &probeOK, &probeErr, &optsText)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nacosrt: enabled sandbox read: %w", err)
	}
	r.Enabled = enabled != 0
	r.LastProbeAt = probeAt.Int64
	r.LastProbeOK = probeOK.Valid && probeOK.Int64 != 0
	r.LastProbeError = probeErr.String
	if r.Options, err = DecodeOptionsText(optsText); err != nil {
		return nil, fmt.Errorf("nacosrt: enabled sandbox read: %w", err)
	}
	return &r, nil
}
