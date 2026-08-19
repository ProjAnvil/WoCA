// Package runqueue defines the run queue interface used by workers to
// claim agent runs, plus a local in-process implementation. Business code
// programs against the Queue interface only; remote implementations
// (e.g. NATS-backed) live in their own subpackages.
package runqueue

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultLease is the lease duration used by NewLocal.
const DefaultLease = 30 * time.Second

// Sentinel errors returned by Queue implementations.
var (
	// ErrNotFound is returned when the referenced run does not exist.
	ErrNotFound = errors.New("runqueue: run not found")
	// ErrNotHeld is returned when the run is not currently held, or is held
	// by a different worker than the caller claims.
	ErrNotHeld = errors.New("runqueue: run not held")
	// ErrDuplicate is returned when a run with the same ID already exists.
	ErrDuplicate = errors.New("runqueue: duplicate run ID")
	// ErrClosed is returned by all methods after the queue is closed.
	ErrClosed = errors.New("runqueue: queue closed")
)

// Run is a unit of work to be executed by a worker.
type Run struct {
	ID        string
	SessionID string
	Payload   []byte
}

// ClaimedRun is a run leased to a worker until LeaseExpiresAt.
type ClaimedRun struct {
	Run            Run
	WorkerID       string
	LeaseExpiresAt time.Time
}

// Queue is a leased work queue. Claim is non-blocking: it returns (nil, nil)
// when no run is available. A claimed run is invisible to other workers until
// its lease expires; the holder renews it with Heartbeat and completes it
// with Ack. Runs whose lease expired are redelivered.
type Queue interface {
	Enqueue(ctx context.Context, run Run) error
	Claim(ctx context.Context, workerID string) (*ClaimedRun, error)
	Ack(ctx context.Context, runID string) error
	Heartbeat(ctx context.Context, runID string, workerID string) error
	Close() error
}

// entry is the internal bookkeeping record for one run.
type entry struct {
	run            Run
	workerID       string
	leaseExpiresAt time.Time
	claimed        bool
	done           bool
}

// Local is an in-process Queue implementation backed by a mutex-protected
// slice. It is safe for concurrent use.
type Local struct {
	mu      sync.Mutex
	lease   time.Duration
	now     func() time.Time
	entries []*entry // FIFO order
	byID    map[string]*entry
	closed  bool
}

var _ Queue = (*Local)(nil)

// NewLocal returns a Local queue with the default 30s lease.
func NewLocal() *Local {
	return NewLocalWithLease(DefaultLease, nil)
}

// NewLocalWithLease returns a Local queue with the given lease duration.
// now is the clock used for lease bookkeeping; nil means time.Now.
func NewLocalWithLease(lease time.Duration, now func() time.Time) *Local {
	if now == nil {
		now = time.Now
	}
	return &Local{
		lease: lease,
		now:   now,
		byID:  make(map[string]*entry),
	}
}

// Enqueue appends run to the back of the queue.
func (l *Local) Enqueue(_ context.Context, run Run) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if _, ok := l.byID[run.ID]; ok {
		return ErrDuplicate
	}
	e := &entry{run: run}
	l.entries = append(l.entries, e)
	l.byID[run.ID] = e
	return nil
}

// Claim leases the earliest enqueued available run to workerID. A run is
// available when it is unclaimed or its lease has expired. Returns
// (nil, nil) when no run is available.
func (l *Local) Claim(_ context.Context, workerID string) (*ClaimedRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	now := l.now()
	for _, e := range l.entries {
		if e.done || (e.claimed && now.Before(e.leaseExpiresAt)) {
			continue
		}
		e.claimed = true
		e.workerID = workerID
		e.leaseExpiresAt = now.Add(l.lease)
		return &ClaimedRun{Run: e.run, WorkerID: workerID, LeaseExpiresAt: e.leaseExpiresAt}, nil
	}
	return nil, nil
}

// Ack completes and removes the run. The run must currently be held under an
// unexpired lease; otherwise ErrNotHeld is returned.
func (l *Local) Ack(_ context.Context, runID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	e, ok := l.byID[runID]
	if !ok {
		return ErrNotFound
	}
	if !e.claimed || !l.now().Before(e.leaseExpiresAt) {
		return ErrNotHeld
	}
	e.done = true
	delete(l.byID, runID)
	return nil
}

// Heartbeat renews the lease on runID for its current holder.
func (l *Local) Heartbeat(_ context.Context, runID string, workerID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	e, ok := l.byID[runID]
	if !ok {
		return ErrNotFound
	}
	if !e.claimed || e.workerID != workerID {
		return ErrNotHeld
	}
	e.leaseExpiresAt = l.now().Add(l.lease)
	return nil
}

// Close marks the queue closed; all further method calls return ErrClosed.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.closed = true
	return nil
}
