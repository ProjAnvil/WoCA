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

	"github.com/projanvil/woca/internal/wocadb"
)

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
		`INSERT INTO runs (run_id, session_id, status, owner, lease_expires_at, config_snapshot, created_at, updated_at)
		 VALUES (`+l.ph(0)+`, `+l.ph(1)+`, '`+StatusQueued+`', '', 0, `+l.ph(2)+`, `+l.ph(3)+`, `+l.ph(4)+`)`,
		run.RunID, run.SessionID, run.ConfigSnapshot, now, now)
	if err != nil && isUniqueViolation(err) {
		return fmt.Errorf("runstate: session %s already has an active run", run.SessionID)
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
	now := l.nowMilli()
	expires := now + lease.Milliseconds()
	q := `UPDATE runs SET status = '` + StatusRunning + `', owner = ` + l.ph(0) +
		`, lease_expires_at = ` + l.ph(1) + `, updated_at = ` + l.ph(2) +
		` WHERE run_id = (SELECT run_id FROM runs WHERE status = '` + StatusQueued +
		`' ORDER BY updated_at, created_at LIMIT 1) RETURNING run_id, session_id, config_snapshot, created_at`
	if l.dialect != wocadb.DialectPostgres {
		// sqlite has no UPDATE...RETURNING on every driver version used here;
		// select-then-update inside a transaction is equivalent because the
		// whole ledger is a single database with its own locking.
		return l.claimSQLite(ctx, workerID, expires, now)
	}
	var run Run
	err := l.db.DB.QueryRowContext(ctx, q, workerID, expires, now).
		Scan(&run.RunID, &run.SessionID, &run.ConfigSnapshot, &run.CreatedAt)
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
		`SELECT run_id, session_id, config_snapshot, created_at FROM runs
		 WHERE status = '`+StatusQueued+`' ORDER BY updated_at, created_at LIMIT 1`).
		Scan(&run.RunID, &run.SessionID, &run.ConfigSnapshot, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET status = '`+StatusRunning+`', owner = ?, lease_expires_at = ?, updated_at = ? WHERE run_id = ?`,
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
		`SELECT run_id, session_id, status, owner, lease_expires_at, config_snapshot, created_at, updated_at
		 FROM runs WHERE session_id = `+l.ph(0)+` AND status IN ('`+StatusQueued+`','`+StatusRunning+`')`,
		sessionID).
		Scan(&run.RunID, &run.SessionID, &run.Status, &run.Owner, &run.LeaseExpiresAt,
			&run.ConfigSnapshot, &run.CreatedAt, &run.UpdatedAt)
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
	defer rows.Close()
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
	rows, err := l.db.DB.QueryContext(ctx,
		`SELECT seq, kind, payload, at FROM run_events
		 WHERE run_id = `+l.ph(0)+` AND seq > `+l.ph(1)+` ORDER BY seq LIMIT `+l.ph(2),
		runID, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		ev.RunID = runID
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Payload, &ev.At); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
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
