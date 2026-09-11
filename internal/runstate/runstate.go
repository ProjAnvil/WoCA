// Package runstate is the Phase 2 externalized run ledger: runs and their
// event streams live in the shared database (design §10/§13 Phase 2), so a
// run is visible across instances, a lease-holding worker that dies has its
// run re-queued by the reconciler, and SSE replay reads run_events directly.
// On postgres the tables are tenant-scoped by RLS via the wocadb GUC
// pipeline; sqlite keeps the single-tenant personal-edition posture.
package runstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

// querier is the subset of *sql.DB/*sql.Tx the ledger uses.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Status values of a run row.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
)

// Run is one externally visible run.
type Run struct {
	RunID          string
	SessionID      string
	Status         string
	Owner          string
	LeaseExpiresAt int64
	ThreadID       string
	ConfigSnapshot string
	CreatedAt      int64
	UpdatedAt      int64
}

// Event is one immutable fact in a run's event stream (seq is assigned by
// Append, per-run and monotonic).
type Event struct {
	RunID   string
	Seq     int64
	Kind    string
	Payload string
	At      int64
}

// ErrLeaseHeld reports an owner/lease mismatch.
var ErrLeaseHeld = errors.New("runstate: run is leased by another owner")

// ErrActiveRun reports a session that already has a live run (admission
// gate); callers classify with errors.Is, never message matching.
var ErrActiveRun = errors.New("runstate: session already has an active run")

// Ledger reads and writes run state on a woca database. It is safe for
// concurrent use.
type Ledger struct {
	db      *wocadb.DB
	dialect wocadb.Dialect
	now     func() time.Time
}

// New returns a Ledger over db (tables are created by wocadb's migration).
func New(db *wocadb.DB) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("runstate: nil database")
	}
	return &Ledger{db: db, dialect: db.Dialect, now: time.Now}, nil
}

// begin returns the querier for one ledger call. Non-local tenants on
// postgres run inside a transaction carrying SET LOCAL app.tenant_id (the
// GUC the runs/run_events RLS policies compare against), mirroring the
// store package's Phase-1 pattern; sqlite and the local tenant use the
// shared pool.
func (l *Ledger) begin(ctx context.Context) (querier, func(bool) error, error) {
	tc, _ := tenancy.FromContext(ctx)
	if tc.TenantID == "" || tc.TenantID == "local" || l.dialect != wocadb.DialectPostgres {
		return l.db.DB, func(bool) error { return nil }, nil
	}
	tx, err := l.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("runstate: tenant tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', `+l.ph(0)+`, true)`, tc.TenantID); err != nil {
		_ = tx.Rollback()
		return nil, nil, fmt.Errorf("runstate: tenant tx scope: %w", err)
	}
	return tx, func(ok bool) error {
		if ok {
			return tx.Commit()
		}
		return tx.Rollback()
	}, nil
}

// PutEvent stores one run event under an explicit sequence number — the
// server's SSE id and the ledger row must agree, so mirroring callers bring
// their own seq (Append mints one instead for telemetry/worker events). A
// duplicate (run_id, seq) with the same kind is treated as an idempotent
// retry.
func (l *Ledger) PutEvent(ctx context.Context, runID string, seq int64, kind, payload string) error {
	if kind == "" {
		return errors.New("runstate: event kind is required")
	}
	q, finish, err := l.begin(ctx)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO run_events (run_id, seq, kind, payload, at) VALUES (`+
			l.ph(0)+`, `+l.ph(1)+`, `+l.ph(2)+`, `+l.ph(3)+`, `+l.ph(4)+`)`,
		runID, seq, kind, payload, l.nowMilli())
	if err != nil && isUniqueViolation(err) {
		var existing string
		if err2 := q.QueryRowContext(ctx,
			`SELECT kind FROM run_events WHERE run_id = `+l.ph(0)+` AND seq = `+l.ph(1),
			runID, seq).Scan(&existing); err2 == nil && existing == kind {
			_ = finish(false)
			return nil
		}
	}
	if err != nil {
		_ = finish(false)
		return err
	}
	return finish(true)
}

func (l *Ledger) ph(i int) string {
	if l.dialect == wocadb.DialectPostgres {
		return fmt.Sprintf("$%d", i+1)
	}
	return "?"
}

func (l *Ledger) nowMilli() int64 { return l.now().UnixMilli() }

// Enqueue inserts a queued run. A second active run for the same session is
// rejected by the one_active_run_per_session partial unique index.
func (l *Ledger) Enqueue(ctx context.Context, run Run) error {
	if run.RunID == "" || run.SessionID == "" {
		return errors.New("runstate: run id and session id are required")
	}
	now := l.nowMilli()
	_, err := l.db.DB.ExecContext(ctx,
		`INSERT INTO runs (run_id, session_id, status, owner, lease_expires_at, thread_id, config_snapshot, created_at, updated_at)
		 VALUES (`+l.ph(0)+`, `+l.ph(1)+`, '`+StatusQueued+`', '', 0, `+l.ph(2)+`, `+l.ph(3)+`, `+l.ph(4)+`, `+l.ph(5)+`)`,
		run.RunID, run.SessionID, run.ThreadID, run.ConfigSnapshot, now, now)
	if err != nil && isUniqueViolation(err) {
		if !isSessionConstraint(err) {
			return fmt.Errorf("runstate: run id %s already exists (cross-process id collision)", run.RunID)
		}
		return fmt.Errorf("%w (session %s)", ErrActiveRun, run.SessionID)
	}
	return err
}

// Claim atomically takes the oldest queued run (or one re-queued by the
// reconciler) under a lease for workerID. It returns (nil, nil) when the
// queue is empty.
func (l *Ledger) Claim(ctx context.Context, workerID string, lease time.Duration) (*Run, error) {
	if workerID == "" {
		return nil, errors.New("runstate: worker id is required")
	}
	// Single-statement claim: newest-updated queued row wins; the lease
	// columns flip in the same write, so two racing workers cannot both win.
	// The outer WHERE re-checks status = queued: under READ COMMITTED, two
	// racing UPDATEs can both select the same queued row from the subquery
	// (each with its own snapshot); the loser then blocks on the winner's
	// row lock, and without the status re-check EvalPlanQual would let it
	// steal the already-running row (observed live: two workers claimed one
	// run in the same millisecond and double-executed it).
	now := l.nowMilli()
	expires := now + lease.Milliseconds()
	q := `UPDATE runs SET status = '` + StatusRunning + `', owner = ` + l.ph(0) +
		`, lease_expires_at = ` + l.ph(1) + `, updated_at = ` + l.ph(2) +
		` WHERE run_id = (SELECT run_id FROM runs WHERE status = '` + StatusQueued +
		`' ORDER BY updated_at, created_at LIMIT 1) AND status = '` + StatusQueued +
		`' RETURNING run_id, session_id, thread_id, config_snapshot, created_at`
	if l.dialect != wocadb.DialectPostgres {
		// sqlite has no UPDATE...RETURNING on every driver version used here;
		// select-then-update inside a transaction is equivalent because the
		// whole ledger is a single database with its own locking.
		return l.claimSQLite(ctx, workerID, expires, now)
	}
	var run Run
	err := l.db.DB.QueryRowContext(ctx, q, workerID, expires, now).
		Scan(&run.RunID, &run.SessionID, &run.ThreadID, &run.ConfigSnapshot, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runstate: claim: %w", err)
	}
	run.Status = StatusRunning
	run.Owner = workerID
	run.LeaseExpiresAt = expires
	run.UpdatedAt = now
	return &run, nil
}

func (l *Ledger) claimSQLite(ctx context.Context, workerID string, expires, now int64) (*Run, error) {
	tx, err := l.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var run Run
	err = tx.QueryRowContext(ctx,
		`SELECT run_id, session_id, thread_id, config_snapshot, created_at FROM runs
		 WHERE status = '`+StatusQueued+`' ORDER BY updated_at, created_at LIMIT 1`).
		Scan(&run.RunID, &run.SessionID, &run.ThreadID, &run.ConfigSnapshot, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET status = '`+StatusRunning+`', owner = ?, lease_expires_at = ?, updated_at = ? WHERE run_id = ? AND status = '`+StatusQueued+`'`,
		workerID, expires, now, run.RunID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	run.Status, run.Owner, run.LeaseExpiresAt, run.UpdatedAt = StatusRunning, workerID, expires, now
	return &run, nil
}

// Heartbeat extends the lease of a run its owner still holds.
func (l *Ledger) Heartbeat(ctx context.Context, runID, workerID string, lease time.Duration) error {
	now := l.nowMilli()
	res, err := l.db.DB.ExecContext(ctx,
		`UPDATE runs SET lease_expires_at = `+l.ph(0)+`, updated_at = `+l.ph(1)+
			` WHERE run_id = `+l.ph(2)+` AND owner = `+l.ph(3)+` AND status = '`+StatusRunning+`'`,
		now+lease.Milliseconds(), now, runID, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Finish records a terminal status and releases the run. Only the lease
// owner may finish a running run; terminal runs stay terminal.
func (l *Ledger) Finish(ctx context.Context, runID, workerID, status string) error {
	res, err := l.db.DB.ExecContext(ctx,
		`UPDATE runs SET status = `+l.ph(0)+`, owner = `+l.ph(1)+`, lease_expires_at = 0, updated_at = `+l.ph(2)+
			` WHERE run_id = `+l.ph(3)+` AND status = '`+StatusRunning+`' AND owner = `+l.ph(4),
		status, workerID, l.nowMilli(), runID, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// ActiveRun returns the queued or running run of a session, if any.
func (l *Ledger) ActiveRun(ctx context.Context, sessionID string) (*Run, bool, error) {
	var run Run
	err := l.db.DB.QueryRowContext(ctx,
		`SELECT run_id, session_id, status, owner, lease_expires_at, thread_id, config_snapshot, created_at, updated_at
		 FROM runs WHERE session_id = `+l.ph(0)+` AND status IN ('`+StatusQueued+`','`+StatusRunning+`')`,
		sessionID).
		Scan(&run.RunID, &run.SessionID, &run.Status, &run.Owner, &run.LeaseExpiresAt,
			&run.ThreadID, &run.ConfigSnapshot, &run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &run, true, nil
}

// ReapExpired re-queues running runs whose lease has lapsed (the worker
// died) and returns their ids. The next Claim hands them out again.
func (l *Ledger) ReapExpired(ctx context.Context) ([]string, error) {
	now := l.nowMilli()
	rows, err := l.db.DB.QueryContext(ctx,
		`UPDATE runs SET status = '`+StatusQueued+`', owner = '', lease_expires_at = 0, updated_at = `+l.ph(0)+
			` WHERE status = '`+StatusRunning+`' AND lease_expires_at != 0 AND lease_expires_at < `+l.ph(1)+
			` RETURNING run_id`, now, now)
	if err != nil {
		return nil, fmt.Errorf("runstate: reap: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Append adds one event to a run's stream, assigning the next per-run seq.
func (l *Ledger) Append(ctx context.Context, runID string, ev Event) (int64, error) {
	if ev.Kind == "" {
		return 0, errors.New("runstate: event kind is required")
	}
	for attempt := 0; attempt < 5; attempt++ {
		var next int64
		err := l.db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = `+l.ph(0), runID).Scan(&next)
		if err != nil {
			return 0, err
		}
		var exists bool
		if err := l.db.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM runs WHERE run_id = `+l.ph(0)+`)`, runID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("runstate: unknown run %s", runID)
		}
		_, err = l.db.DB.ExecContext(ctx,
			`INSERT INTO run_events (run_id, seq, kind, payload, at) VALUES (`+
				l.ph(0)+`, `+l.ph(1)+`, `+l.ph(2)+`, `+l.ph(3)+`, `+l.ph(4)+`)`,
			runID, next, ev.Kind, ev.Payload, l.nowMilli())
		if err != nil {
			if isUniqueViolation(err) && attempt < 4 {
				continue // concurrent append won the seq; retry with the next
			}
			return 0, err
		}
		return next, nil
	}
	return 0, errors.New("runstate: append: seq contention")
}

// Replay returns the run's events with seq > sinceSeq in order.
func (l *Ledger) Replay(ctx context.Context, runID string, sinceSeq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	q, finish, err := l.begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT seq, kind, payload, at FROM run_events
		 WHERE run_id = `+l.ph(0)+` AND seq > `+l.ph(1)+` ORDER BY seq LIMIT `+l.ph(2),
		runID, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var ev Event
		ev.RunID = runID
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Payload, &ev.At); err != nil {
			_ = finish(false)
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		_ = finish(false)
		return nil, err
	}
	return out, finish(true)
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "23505")
}

// isSessionConstraint reports whether a unique violation came from the
// one_active_run_per_session partial index (as opposed to the run_id
// primary key — a cross-process id collision).
func isSessionConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "one_active_run_per_session") ||
		strings.Contains(msg, "runs.session_id")
}

// Row returns the recorded state of one run (read-only accessor).
func (l *Ledger) Row(ctx context.Context, runID string) (Run, bool, error) {
	q, finish, err := l.begin(ctx)
	if err != nil {
		return Run{}, false, err
	}
	var run Run
	err = q.QueryRowContext(ctx,
		`SELECT run_id, session_id, status, owner, lease_expires_at, thread_id, config_snapshot, created_at, updated_at
		 FROM runs WHERE run_id = `+l.ph(0), runID).
		Scan(&run.RunID, &run.SessionID, &run.Status, &run.Owner, &run.LeaseExpiresAt,
			&run.ThreadID, &run.ConfigSnapshot, &run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		_ = finish(false)
		return Run{}, false, nil
	}
	if err != nil {
		_ = finish(false)
		return Run{}, false, err
	}
	return run, true, finish(true)
}

// Start registers a run this caller executes itself: the row is born
// running under workerID's lease (the one_active_run_per_session constraint
// still applies). Workers that pick runs up from a queue use Enqueue/Claim
// instead.
func (l *Ledger) Start(ctx context.Context, run Run, workerID string, lease time.Duration) error {
	if run.RunID == "" || run.SessionID == "" {
		return errors.New("runstate: run id and session id are required")
	}
	now := l.nowMilli()
	_, err := l.db.DB.ExecContext(ctx,
		`INSERT INTO runs (run_id, session_id, status, owner, lease_expires_at, thread_id, config_snapshot, created_at, updated_at)
		 VALUES (`+l.ph(0)+`, `+l.ph(1)+`, '`+StatusRunning+`', `+l.ph(2)+`, `+l.ph(3)+`, `+l.ph(4)+`, `+l.ph(5)+`, `+l.ph(6)+`, `+l.ph(7)+`)`,
		run.RunID, run.SessionID, workerID, now+lease.Milliseconds(), run.ThreadID, run.ConfigSnapshot, now, now)
	if err != nil && isUniqueViolation(err) {
		if !isSessionConstraint(err) {
			return fmt.Errorf("runstate: run id %s already exists (cross-process id collision)", run.RunID)
		}
		return fmt.Errorf("%w (session %s)", ErrActiveRun, run.SessionID)
	}
	return err
}
