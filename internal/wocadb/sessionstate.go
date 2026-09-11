package wocadb

import (
	"fmt"
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

// SetSessionState upserts one session's run state.
func (d *DB) SetSessionState(st SessionState) error {
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
	_, err := d.Exec(query, st.SessionID, st.Status, st.RunID, st.Owner, st.UpdatedAt)
	if err != nil {
		return fmt.Errorf("wocadb: set session_state %s: %w", st.SessionID, err)
	}
	return nil
}

// ClearSessionState deletes a session's row only while it still points at
// runID (compare-and-clear). This keeps a finished run from clearing the row
// of the queued run promoted behind it. It reports whether a row was removed.
func (d *DB) ClearSessionState(sessionID, runID string) (bool, error) {
	res, err := d.Exec(
		fmt.Sprintf(`DELETE FROM session_state WHERE session_id = %s AND run_id = %s`, d.ph(0), d.ph(1)),
		sessionID, runID)
	if err != nil {
		return false, fmt.Errorf("wocadb: clear session_state %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteSessionState drops a session's row unconditionally (session deleted).
func (d *DB) DeleteSessionState(sessionID string) error {
	if _, err := d.Exec(
		fmt.Sprintf(`DELETE FROM session_state WHERE session_id = %s`, d.ph(0)), sessionID); err != nil {
		return fmt.Errorf("wocadb: delete session_state %s: %w", sessionID, err)
	}
	return nil
}

// SessionStates returns every recorded row (empty when none).
func (d *DB) SessionStates() ([]SessionState, error) {
	rows, err := d.Query(`SELECT session_id, status, run_id, owner, updated_at FROM session_state`)
	if err != nil {
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
	return out, rows.Err()
}
