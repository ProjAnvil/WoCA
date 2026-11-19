package wocadb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// ph returns the i-th (0-based) SQL argument placeholder for the dialect.
func (d *DB) ph(i int) string {
	if d.Dialect == DialectPostgres {
		return fmt.Sprintf("$%d", i+1)
	}
	return "?"
}

// Session-state statuses recorded in the session_state table.
const (
	// SessionRunning marks a session whose run holds the admission gate and
	// is actively executing.
	SessionRunning = "running"
	// SessionAwaiting marks a session whose run is parked on an unanswered
	// interrupt (permission / ask / plan approval).
	SessionAwaiting = "awaiting"
)

// SessionState is one session's durable run state. The owner ("pid@host")
// stamps which process recorded it so a restart can reconcile rows whose
// process died mid-run (crash between mark and clear).
type SessionState struct {
	SessionID string
	Status    string // SessionRunning | SessionAwaiting
	RunID     string
	Owner     string
	UpdatedAt int64
}

// ctxQuerier is the statement surface shared by *sql.DB and *sql.Tx for
// tenant-scoped session_state calls.
type ctxQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// beginTenant returns the querier for one session_state call. Non-local
// tenants on postgres run inside a transaction carrying SET LOCAL
// app.tenant_id — the same tenant-transaction prologue runstate's ledger
// and the store package use — so the busy rows a gateway or worker writes
// land under the owning tenant and that tenant's readers (session lists,
// the global event snapshot) see them through the FORCE RLS policy instead
// of the writes defaulting to 'local'. Sqlite and the local tenant keep the
// shared pool (single-tenant backends, unchanged).
func (d *DB) beginTenant(ctx context.Context) (ctxQuerier, func(bool) error, error) {
	tc, _ := tenancy.FromContext(ctx)
	if tc.TenantID == "" || tc.TenantID == "local" || d.Dialect != DialectPostgres {
		return d.DB, func(bool) error { return nil }, nil
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("wocadb: session_state tenant tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', `+d.ph(0)+`, true)`, tc.TenantID); err != nil {
		_ = tx.Rollback()
		return nil, nil, fmt.Errorf("wocadb: session_state tenant tx scope: %w", err)
	}
	return tx, func(ok bool) error {
		if ok {
			return tx.Commit()
		}
		return tx.Rollback()
	}, nil
}

// SetSessionState upserts one session's run state.
func (d *DB) SetSessionState(ctx context.Context, st SessionState) error {
	var query string
	switch d.Dialect {
	case DialectPostgres:
		query = `INSERT INTO session_state (session_id, status, run_id, owner, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (session_id) DO UPDATE SET
				status = EXCLUDED.status, run_id = EXCLUDED.run_id,
				owner = EXCLUDED.owner, updated_at = EXCLUDED.updated_at`
	default:
		query = `INSERT INTO session_state (session_id, status, run_id, owner, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (session_id) DO UPDATE SET
				status = excluded.status, run_id = excluded.run_id,
				owner = excluded.owner, updated_at = excluded.updated_at`
	}
	q, finish, err := d.beginTenant(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, query, st.SessionID, st.Status, st.RunID, st.Owner, st.UpdatedAt); err != nil {
		_ = finish(false)
		return fmt.Errorf("wocadb: set session_state %s: %w", st.SessionID, err)
	}
	return finish(true)
}

// ClearSessionState deletes a session's row only while it still points at
// runID (compare-and-clear). This keeps a finished run from clearing the row
// of the queued run promoted behind it. It reports whether a row was removed.
func (d *DB) ClearSessionState(ctx context.Context, sessionID, runID string) (bool, error) {
	q, finish, err := d.beginTenant(ctx)
	if err != nil {
		return false, err
	}
	res, err := q.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM session_state WHERE session_id = %s AND run_id = %s`, d.ph(0), d.ph(1)),
		sessionID, runID)
	if err != nil {
		_ = finish(false)
		return false, fmt.Errorf("wocadb: clear session_state %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	if err := finish(true); err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteSessionState drops a session's row unconditionally (session deleted).
func (d *DB) DeleteSessionState(ctx context.Context, sessionID string) error {
	q, finish, err := d.beginTenant(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM session_state WHERE session_id = %s`, d.ph(0)), sessionID); err != nil {
		_ = finish(false)
		return fmt.Errorf("wocadb: delete session_state %s: %w", sessionID, err)
	}
	return finish(true)
}

// SessionStates returns every recorded row the caller's tenant may see
// (empty when none). A tenant-carrying context on postgres reads through
// the tenant transaction, so a tenant's snapshot contains exactly its own
// rows; tenantless callers keep the 'local' default's view.
func (d *DB) SessionStates(ctx context.Context) ([]SessionState, error) {
	q, finish, err := d.beginTenant(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT session_id, status, run_id, owner, updated_at FROM session_state`)
	if err != nil {
		_ = finish(false)
		return nil, fmt.Errorf("wocadb: list session_state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionState
	for rows.Next() {
		var st SessionState
		if err := rows.Scan(&st.SessionID, &st.Status, &st.RunID, &st.Owner, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		_ = finish(false)
		return nil, err
	}
	return out, finish(true)
}
