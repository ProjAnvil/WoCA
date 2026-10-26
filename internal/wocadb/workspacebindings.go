package wocadb

import (
	"database/sql"
	"errors"
	"fmt"
)

// WorkspaceBindingActive is the state of a workspace bound to an executor
// and eligible to receive runs (design §7.1). Other states (e.g. detached
// during executor drain) are set by later lifecycle steps.
const WorkspaceBindingActive = "active"

// WorkspaceBinding pins one workspace to one executor across runs (design
// §7.1): the binding outlives individual run rows, so a workspace's files
// stay on the executor that already holds them. SourceKey is the lookup key
// (tenant_id + normalized source path) that makes allocate-or-bind
// idempotent: two sessions over the same workspace resolve to the same ID.
type WorkspaceBinding struct {
	WorkspaceID string
	SourceKey   string
	TenantID    string
	ExecutorID  string
	State       string // WorkspaceBindingActive | …
	CreatedAt   int64
	UpdatedAt   int64
}

// scanWorkspaceBindingColumns is the SELECT column list for workspace_bindings,
// in scan order.
const workspaceBindingColumns = `workspace_id, source_key, tenant_id, executor_id, state, created_at, updated_at`

// EnsureWorkspaceBinding allocates-or-binds: it inserts the binding and, when
// the source_key already exists, keeps the existing row (ON CONFLICT DO
// NOTHING) and returns it. Concurrent calls with the same source_key all
// observe the first row, so workspace IDs are allocated exactly once per
// source. Zero CreatedAt/UpdatedAt are stamped; an empty state defaults to
// active.
func (d *DB) EnsureWorkspaceBinding(b WorkspaceBinding) (WorkspaceBinding, error) {
	now := Now()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	if b.UpdatedAt == 0 {
		b.UpdatedAt = now
	}
	if b.State == "" {
		b.State = WorkspaceBindingActive
	}
	var query string
	switch d.Dialect {
	case DialectPostgres:
		// Placeholders verified by hand: $1 workspace_id … $7 updated_at,
		// matching the argument order below (the sqlite "?" form cannot
		// catch an off-by-one).
		query = `INSERT INTO workspace_bindings (` + workspaceBindingColumns + `)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (source_key) DO NOTHING`
	default:
		query = `INSERT INTO workspace_bindings (` + workspaceBindingColumns + `)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (source_key) DO NOTHING`
	}
	if _, err := d.Exec(query,
		b.WorkspaceID, b.SourceKey, b.TenantID, b.ExecutorID, b.State, b.CreatedAt, b.UpdatedAt,
	); err != nil {
		return WorkspaceBinding{}, fmt.Errorf("wocadb: ensure workspace_binding %s: %w", b.SourceKey, err)
	}
	got, ok, err := d.WorkspaceBindingBySourceKey(b.SourceKey)
	if err != nil {
		return WorkspaceBinding{}, err
	}
	if !ok {
		return WorkspaceBinding{}, fmt.Errorf("wocadb: ensure workspace_binding %s: row missing after insert", b.SourceKey)
	}
	return got, nil
}

// WorkspaceBindingBySourceKey looks a binding up by its allocate-or-bind key.
func (d *DB) WorkspaceBindingBySourceKey(key string) (WorkspaceBinding, bool, error) {
	var b WorkspaceBinding
	err := d.QueryRow(
		fmt.Sprintf(`SELECT %s FROM workspace_bindings WHERE source_key = %s`, workspaceBindingColumns, d.ph(0)),
		key,
	).Scan(&b.WorkspaceID, &b.SourceKey, &b.TenantID, &b.ExecutorID, &b.State, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceBinding{}, false, nil
	}
	if err != nil {
		return WorkspaceBinding{}, false, fmt.Errorf("wocadb: workspace_binding by source_key %s: %w", key, err)
	}
	return b, true, nil
}

// WorkspaceBindingByID looks a binding up by its workspace ID.
func (d *DB) WorkspaceBindingByID(id string) (WorkspaceBinding, bool, error) {
	var b WorkspaceBinding
	err := d.QueryRow(
		fmt.Sprintf(`SELECT %s FROM workspace_bindings WHERE workspace_id = %s`, workspaceBindingColumns, d.ph(0)),
		id,
	).Scan(&b.WorkspaceID, &b.SourceKey, &b.TenantID, &b.ExecutorID, &b.State, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceBinding{}, false, nil
	}
	if err != nil {
		return WorkspaceBinding{}, false, fmt.Errorf("wocadb: workspace_binding by id %s: %w", id, err)
	}
	return b, true, nil
}

// SetWorkspaceBindingState transitions a binding's state and stamps
// updated_at. An unknown ID is an idempotent no-op, not an error.
func (d *DB) SetWorkspaceBindingState(id, state string) error {
	// Placeholders: $1 state, $2 updated_at, $3 workspace_id (postgres).
	if _, err := d.Exec(
		fmt.Sprintf(`UPDATE workspace_bindings SET state = %s, updated_at = %s WHERE workspace_id = %s`,
			d.ph(0), d.ph(1), d.ph(2)),
		state, Now(), id,
	); err != nil {
		return fmt.Errorf("wocadb: set workspace_binding %s state %s: %w", id, state, err)
	}
	return nil
}

// WorkspaceBindings returns every binding ordered by source_key (empty when
// none).
func (d *DB) WorkspaceBindings() ([]WorkspaceBinding, error) {
	rows, err := d.Query(fmt.Sprintf(`SELECT %s FROM workspace_bindings ORDER BY source_key`, workspaceBindingColumns))
	if err != nil {
		return nil, fmt.Errorf("wocadb: list workspace_bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkspaceBinding
	for rows.Next() {
		var b WorkspaceBinding
		if err := rows.Scan(&b.WorkspaceID, &b.SourceKey, &b.TenantID, &b.ExecutorID, &b.State, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
