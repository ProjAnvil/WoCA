// queue_test.go exercises the per-executor bounded queue in isolation:
// FIFO grants, queue-full shedding, queue timeout, tenant quota, and
// disconnect recall.
package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

type acquireResult struct {
	release func()
	err     error
}

func asyncAcquire(q *execQueue, tenant string) chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		rel, err := q.acquire(context.Background(), tenant)
		out <- acquireResult{release: rel, err: err}
	}()
	return out
}

func testQueueLimits() Limits {
	return Limits{PerExecutor: 1, QueueLen: 8, TenantQuota: 4, QueueTimeout: 5 * time.Second}
}

// TestQueueGrantsFIFO: releases hand the slot to the head of the queue, in
// arrival order. (Waiters are queued one at a time — enqueue order is what
// FIFO means here, and concurrent launchers would race their order.)
func TestQueueGrantsFIFO(t *testing.T) {
	q := newExecQueue(testQueueLimits())

	rel1, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	done2 := asyncAcquire(q, "t2")
	waitFor(t, "t2 queued", func() bool { return q.depth("t2") == 1 })
	done3 := asyncAcquire(q, "t3")
	waitFor(t, "t3 queued", func() bool { return q.depth("t3") == 1 })

	rel1()
	r2 := <-done2
	if r2.err != nil {
		t.Fatalf("t2 not granted after first release: %v", r2.err)
	}
	r2.release()
	r3 := <-done3
	if r3.err != nil {
		t.Fatalf("t3 not granted after second release: %v", r3.err)
	}
	r3.release()
	if got := q.inUse(); got != 0 {
		t.Fatalf("in-use after full drain = %d, want 0", got)
	}
}

// TestQueueFullSheds: the bounded queue rejects synchronous overflow.
func TestQueueFullSheds(t *testing.T) {
	q := newExecQueue(Limits{PerExecutor: 1, QueueLen: 1, TenantQuota: 4, QueueTimeout: 5 * time.Second})
	rel, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	done := asyncAcquire(q, "t1") // fills the queue (cap 1)
	waitFor(t, "waiter queued", func() bool { return q.depth("t1") == 1 })

	_, err = q.acquire(context.Background(), "t1")
	if !errors.Is(err, errQueueFull) {
		t.Fatalf("overflow acquire err = %v, want errQueueFull", err)
	}
	rel()
	if r := <-done; r.err != nil {
		t.Fatalf("queued waiter failed: %v", r.err)
	} else {
		r.release()
	}
}

// TestQueueTimeout: a waiter outstaying QueueTimeout errors and leaves the
// queue; the slot accounting stays balanced.
func TestQueueTimeout(t *testing.T) {
	q := newExecQueue(Limits{PerExecutor: 1, QueueLen: 8, TenantQuota: 4, QueueTimeout: 60 * time.Millisecond})
	rel, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.acquire(context.Background(), "t2")
	if !errors.Is(err, errQueueTimeout) {
		t.Fatalf("timed-out acquire err = %v, want errQueueTimeout", err)
	}
	if q.depth("t2") != 0 {
		t.Fatalf("timed-out waiter still counted: depth=%d", q.depth("t2"))
	}
	rel()
	if got := q.inUse(); got != 0 {
		t.Fatalf("in-use after drain = %d, want 0", got)
	}
}

// TestQueueTenantQuota: per-tenant queued-waiter cap rejects the tenant's
// next waiter while a foreign tenant still enqueues.
func TestQueueTenantQuota(t *testing.T) {
	q := newExecQueue(Limits{PerExecutor: 1, QueueLen: 8, TenantQuota: 2, QueueTimeout: 5 * time.Second})
	rel, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	// Queue waiters one at a time: the grant order must be deterministic.
	d2 := asyncAcquire(q, "t-a")
	waitFor(t, "first t-a waiter queued", func() bool { return q.depth("t-a") == 1 })
	d3 := asyncAcquire(q, "t-a")
	waitFor(t, "tenant t-a fills its quota", func() bool { return q.depth("t-a") == 2 })

	if _, err := q.acquire(context.Background(), "t-a"); !errors.Is(err, errTenantQuota) {
		t.Fatalf("over-quota acquire err = %v, want errTenantQuota", err)
	}
	d4 := asyncAcquire(q, "t-b")
	waitFor(t, "foreign tenant queues", func() bool { return q.depth("t-b") == 1 })

	rel()
	for _, d := range []chan acquireResult{d2, d3, d4} {
		if r := <-d; r.err != nil {
			t.Fatalf("queued waiter failed: %v", r.err)
		} else {
			r.release()
		}
	}
}

// TestQueueCancelRecalls: a cancelled waiter leaves the queue and never
// consumes a slot; the next waiter is granted instead.
func TestQueueCancelRecalls(t *testing.T) {
	q := newExecQueue(testQueueLimits())
	rel, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := asyncAcquireCtx(ctx, q, "t2")
	waitFor(t, "waiter queued", func() bool { return q.depth("t2") == 1 })

	cancel()
	if r := <-done; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v, want context.Canceled", r.err)
	}
	if q.depth("t2") != 0 {
		t.Fatalf("cancelled waiter still counted: depth=%d", q.depth("t2"))
	}

	// The slot the cancelled waiter would have taken goes to the next one.
	next := asyncAcquire(q, "t3")
	waitFor(t, "next waiter queued", func() bool { return q.depth("t3") == 1 })
	rel()
	if r := <-next; r.err != nil {
		t.Fatalf("next waiter not granted: %v", r.err)
	} else {
		r.release()
	}
	if got := q.inUse(); got != 0 {
		t.Fatalf("in-use after drain = %d, want 0", got)
	}
}

// TestQueueFastPath: with a free slot, acquire succeeds immediately and the
// waiter never touches the queue.
func TestQueueFastPath(t *testing.T) {
	q := newExecQueue(Limits{PerExecutor: 2, QueueLen: 8, TenantQuota: 4, QueueTimeout: 5 * time.Second})
	rel, err := q.acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if q.depth("t1") != 0 || q.inUse() != 1 {
		t.Fatalf("fast path: depth=%d inUse=%d, want 0/1", q.depth("t1"), q.inUse())
	}
	rel()
	if q.inUse() != 0 {
		t.Fatalf("release left inUse=%d", q.inUse())
	}
}

// TestQueueSetLazilyBuildsPerExecutor: one queue per executor id, shared.
func TestQueueSetLazilyBuildsPerExecutor(t *testing.T) {
	s := newQueueSet(Limits{PerExecutor: 2, QueueLen: 8, TenantQuota: 4, QueueTimeout: 5 * time.Second})
	first := s.get("e1")
	if s.get("e1") != first {
		t.Fatal("queue set rebuilt an existing executor queue")
	}
	if s.get("e1") == s.get("e2") {
		t.Fatal("executors share a queue")
	}
}

func asyncAcquireCtx(ctx context.Context, q *execQueue, tenant string) chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		rel, err := q.acquire(ctx, tenant)
		out <- acquireResult{release: rel, err: err}
	}()
	return out
}
