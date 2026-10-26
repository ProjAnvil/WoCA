package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Queue-pressure failures. All shed with 503: overload is shed, never
// buffered (a tar-class request parked in proxy memory is an OOM).
var (
	errQueueFull    = errors.New("executor queue full")
	errQueueTimeout = errors.New("queue wait timed out")
	errTenantQuota  = errors.New("tenant queue quota exceeded")
)

// queueSet owns one execQueue per executor id, built lazily on first use.
type queueSet struct {
	mu     sync.Mutex
	queues map[string]*execQueue
	limits Limits
}

func newQueueSet(l Limits) *queueSet {
	return &queueSet{queues: map[string]*execQueue{}, limits: l}
}

func (s *queueSet) get(executorID string) *execQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[executorID]
	if q == nil {
		q = newExecQueue(s.limits)
		s.queues[executorID] = q
	}
	return q
}

// acquire admits one request on the named executor's bounded queue.
func (s *queueSet) acquire(ctx context.Context, executorID, tenant string) (func(), error) {
	return s.get(executorID).acquire(ctx, tenant)
}

// execQueue is one executor's admission control: a counting semaphore (cap
// PerExecutor) for in-flight requests plus a bounded FIFO of waiters, with
// per-tenant queued-waiter quotas and a queue timeout.
//
// Synchronization: every semaphore send and every queue grant happens under
// mu, which maintains the invariant "a free slot coexists with a queued
// waiter never" — a release either transfers its slot directly to the head
// waiter (no channel traffic, the token simply changes owners) or returns
// it only after the queue drained. The fast path therefore can never barge
// past queued waiters, and grants are FIFO without a dispatcher goroutine.
type execQueue struct {
	sem     chan struct{}
	q       chan *waiter
	mu      sync.Mutex
	queued  map[string]int // tenant → queued waiter count (quota metering)
	quota   int
	timeout time.Duration
}

func newExecQueue(l Limits) *execQueue {
	return &execQueue{
		sem:     make(chan struct{}, l.PerExecutor),
		q:       make(chan *waiter, l.QueueLen),
		queued:  map[string]int{},
		quota:   l.TenantQuota,
		timeout: l.QueueTimeout,
	}
}

// waiter is one queued admission request. state moves Queued → Granted
// (a release handed it a slot) or Queued → Recalled (its request went away
// first); both transitions are CASes so grant and recall never double-apply.
type waiter struct {
	tenant string
	ready  chan struct{} // closed on grant
	state  atomic.Int32
	// counted guards the single decrement of execQueue.queued (granted and
	// recalled paths both run under e.mu).
	counted bool
}

const (
	waiterQueued int32 = iota
	waiterGranted
	waiterRecalled
)

// acquire admits the caller or fails. Failures: errTenantQuota (the tenant
// already holds its quota of queued waiters), errQueueFull (the bounded
// queue is full), errQueueTimeout (the wait outstayed QueueTimeout), or the
// context error (client disconnect — the queue position is returned).
//
// Disconnect detection rides the request context. net/http only arms its
// connection-close detection once a request's body is consumed, so a
// queued request that still carries its (streamed) body learns of a dead
// client at the queue timeout at the latest — the timeout is the bound,
// and QueueTimeout must stay well under the backend's per-call timeout.
func (e *execQueue) acquire(ctx context.Context, tenant string) (func(), error) {
	e.mu.Lock()
	// Fast path: a free slot under the lock. The mutex invariant guarantees
	// no waiter is queued when a slot is free, so this cannot barge.
	select {
	case e.sem <- struct{}{}:
		e.mu.Unlock()
		return e.release, nil
	default:
	}
	if e.queued[tenant] >= e.quota {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w (tenant %s holds %d queued, max %d)", errTenantQuota, tenant, e.queued[tenant], e.quota)
	}
	w := &waiter{tenant: tenant, ready: make(chan struct{})}
	w.state.Store(waiterQueued)
	select {
	case e.q <- w:
		e.queued[tenant]++
		w.counted = true
		e.mu.Unlock()
	default:
		e.mu.Unlock()
		return nil, fmt.Errorf("%w (concurrency %d, queue %d)", errQueueFull, cap(e.sem), cap(e.q))
	}

	timer := time.NewTimer(e.timeout)
	defer timer.Stop()
	select {
	case <-w.ready:
		return e.release, nil
	case <-ctx.Done():
		if e.recall(w) {
			return nil, ctx.Err()
		}
		// The grant won the race: a slot is ours — consume it and hand it
		// straight back so the queue keeps draining.
		<-w.ready
		e.release()
		return nil, ctx.Err()
	case <-timer.C:
		if e.recall(w) {
			return nil, fmt.Errorf("%w after %s", errQueueTimeout, e.timeout)
		}
		<-w.ready
		e.release()
		return nil, fmt.Errorf("%w after %s", errQueueTimeout, e.timeout)
	}
}

// recall reclaims a waiter that gave up. It reports false when the waiter
// was granted concurrently — the caller must then consume w.ready and
// release the slot itself.
func (e *execQueue) recall(w *waiter) bool {
	if !w.state.CompareAndSwap(waiterQueued, waiterRecalled) {
		return false
	}
	e.mu.Lock()
	e.uncountLocked(w)
	e.mu.Unlock()
	return true
}

// release returns one held slot: directly to the head of the queue (the
// token changes owners without channel traffic) or, when nobody waits, to
// the semaphore.
func (e *execQueue) release() {
	e.mu.Lock()
	for {
		select {
		case w := <-e.q:
			if w.state.CompareAndSwap(waiterQueued, waiterGranted) {
				e.uncountLocked(w)
				close(w.ready)
				e.mu.Unlock()
				return
			}
			// A recalled corpse (its request went away): drop it and keep
			// handing the slot down the queue.
		default:
			// Queue drained: return the token. Non-blocking because every
			// release corresponds to a held token; the default arm only
			// guards against a double release corrupting the count.
			select {
			case <-e.sem:
			default:
			}
			e.mu.Unlock()
			return
		}
	}
}

// uncountLocked decrements the tenant's queued-waiter meter exactly once
// per waiter. Caller holds e.mu.
func (e *execQueue) uncountLocked(w *waiter) {
	if !w.counted {
		return
	}
	w.counted = false
	if n := e.queued[w.tenant] - 1; n > 0 {
		e.queued[w.tenant] = n
	} else {
		delete(e.queued, w.tenant)
	}
}

// depth reports the tenant's queued waiters (tests and introspection).
func (e *execQueue) depth(tenant string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queued[tenant]
}

// inUse reports the in-flight request count (tests and introspection).
func (e *execQueue) inUse() int {
	return len(e.sem)
}
