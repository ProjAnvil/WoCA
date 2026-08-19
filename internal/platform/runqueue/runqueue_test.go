package runqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic lease tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestEnqueueClaimAckFlow(t *testing.T) {
	q := NewLocal()
	ctx := context.Background()

	runs := []Run{
		{ID: "r1", SessionID: "s1", Payload: []byte("a")},
		{ID: "r2", SessionID: "s1", Payload: []byte("b")},
	}
	for _, r := range runs {
		if err := q.Enqueue(ctx, r); err != nil {
			t.Fatalf("Enqueue(%s): %v", r.ID, err)
		}
	}

	// Claims come out in FIFO order.
	for i, want := range runs {
		got, err := q.Claim(ctx, "w1")
		if err != nil {
			t.Fatalf("Claim #%d: %v", i, err)
		}
		if got == nil {
			t.Fatalf("Claim #%d = nil, want run %s", i, want.ID)
		}
		if got.Run.ID != want.ID {
			t.Errorf("Claim #%d run ID = %q, want %q", i, got.Run.ID, want.ID)
		}
		if got.WorkerID != "w1" {
			t.Errorf("Claim #%d worker = %q, want w1", i, got.WorkerID)
		}
		if got.LeaseExpiresAt.IsZero() {
			t.Errorf("Claim #%d lease expiry is zero", i)
		}
		if err := q.Ack(ctx, got.Run.ID); err != nil {
			t.Fatalf("Ack(%s): %v", got.Run.ID, err)
		}
	}

	// Queue is drained.
	got, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("Claim after drain: %v", err)
	}
	if got != nil {
		t.Errorf("Claim after drain = %+v, want nil", got)
	}
}

func TestClaimEmpty(t *testing.T) {
	q := NewLocal()
	got, err := q.Claim(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got != nil {
		t.Errorf("Claim on empty queue = %+v, want nil", got)
	}
}

func TestDefaultLeaseDuration(t *testing.T) {
	q := NewLocal()
	ctx := context.Background()
	before := time.Now()
	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got == nil {
		t.Fatal("Claim = nil, want run")
	}
	ttl := got.LeaseExpiresAt.Sub(before)
	if ttl < 30*time.Second || ttl > 31*time.Second {
		t.Errorf("default lease TTL = %v, want ~30s", ttl)
	}
}

func TestClaimSkipsHeldRun(t *testing.T) {
	clock := newFakeClock()
	q := NewLocalWithLease(30*time.Second, clock.Now)
	ctx := context.Background()

	for _, id := range []string{"r1", "r2"} {
		if err := q.Enqueue(ctx, Run{ID: id}); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	first, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("Claim w1: %v", err)
	}
	if first == nil || first.Run.ID != "r1" {
		t.Fatalf("Claim w1 = %+v, want r1", first)
	}

	// Within the lease another worker cannot take r1; it gets r2.
	second, err := q.Claim(ctx, "w2")
	if err != nil {
		t.Fatalf("Claim w2: %v", err)
	}
	if second == nil || second.Run.ID != "r2" {
		t.Fatalf("Claim w2 = %+v, want r2", second)
	}

	// Both runs are held: nothing left to claim.
	got, err := q.Claim(ctx, "w3")
	if err != nil {
		t.Fatalf("Claim w3: %v", err)
	}
	if got != nil {
		t.Errorf("Claim w3 = %+v, want nil", got)
	}
}

func TestLeaseExpiryRedelivers(t *testing.T) {
	clock := newFakeClock()
	q := NewLocalWithLease(30*time.Second, clock.Now)
	ctx := context.Background()

	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("Claim w1: %v", err)
	}
	if first == nil {
		t.Fatal("Claim w1 = nil, want r1")
	}

	// Same worker cannot re-claim its own leased run either.
	got, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("re-Claim w1: %v", err)
	}
	if got != nil {
		t.Errorf("re-Claim w1 = %+v, want nil (lease held)", got)
	}

	// After the lease expires the run is redelivered to another worker.
	clock.Advance(31 * time.Second)
	second, err := q.Claim(ctx, "w2")
	if err != nil {
		t.Fatalf("Claim w2: %v", err)
	}
	if second == nil || second.Run.ID != "r1" {
		t.Fatalf("Claim w2 = %+v, want redelivered r1", second)
	}
	if second.WorkerID != "w2" {
		t.Errorf("redelivered worker = %q, want w2", second.WorkerID)
	}
	if !second.LeaseExpiresAt.After(first.LeaseExpiresAt) {
		t.Errorf("new lease %v not after old lease %v", second.LeaseExpiresAt, first.LeaseExpiresAt)
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	clock := newFakeClock()
	q := NewLocalWithLease(30*time.Second, clock.Now)
	ctx := context.Background()

	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Claim(ctx, "w1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("Claim = nil, want r1")
	}

	// Advance close to expiry, then renew.
	clock.Advance(20 * time.Second)
	if err := q.Heartbeat(ctx, "r1", "w1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Original lease has expired, but the renewed one still holds.
	clock.Advance(20 * time.Second)
	got, err := q.Claim(ctx, "w2")
	if err != nil {
		t.Fatalf("Claim w2: %v", err)
	}
	if got != nil {
		t.Errorf("Claim w2 = %+v, want nil (lease renewed)", got)
	}

	// Past the renewed lease the run becomes claimable again.
	clock.Advance(11 * time.Second)
	got, err = q.Claim(ctx, "w2")
	if err != nil {
		t.Fatalf("Claim w2 after renewal expiry: %v", err)
	}
	if got == nil || got.Run.ID != "r1" {
		t.Errorf("Claim w2 after renewal expiry = %+v, want r1", got)
	}
}

func TestHeartbeatErrors(t *testing.T) {
	clock := newFakeClock()
	ctx := context.Background()

	cases := []struct {
		name   string
		setup  func(q *Local)
		runID  string
		worker string
		want   error
	}{
		{
			name:   "unknown run",
			setup:  func(q *Local) {},
			runID:  "nope",
			worker: "w1",
			want:   ErrNotFound,
		},
		{
			name: "run not claimed",
			setup: func(q *Local) {
				_ = q.Enqueue(ctx, Run{ID: "r1"})
			},
			runID:  "r1",
			worker: "w1",
			want:   ErrNotHeld,
		},
		{
			name: "held by another worker",
			setup: func(q *Local) {
				_ = q.Enqueue(ctx, Run{ID: "r1"})
				_, _ = q.Claim(ctx, "w1")
			},
			runID:  "r1",
			worker: "w2",
			want:   ErrNotHeld,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := NewLocalWithLease(30*time.Second, clock.Now)
			c.setup(q)
			err := q.Heartbeat(ctx, c.runID, c.worker)
			if !errors.Is(err, c.want) {
				t.Errorf("Heartbeat = %v, want %v", err, c.want)
			}
		})
	}
}

func TestAckErrors(t *testing.T) {
	clock := newFakeClock()
	ctx := context.Background()

	t.Run("unknown run", func(t *testing.T) {
		q := NewLocalWithLease(30*time.Second, clock.Now)
		if err := q.Ack(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Ack = %v, want ErrNotFound", err)
		}
	})

	t.Run("run not claimed", func(t *testing.T) {
		q := NewLocalWithLease(30*time.Second, clock.Now)
		if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := q.Ack(ctx, "r1"); !errors.Is(err, ErrNotHeld) {
			t.Errorf("Ack = %v, want ErrNotHeld", err)
		}
	})

	t.Run("lease expired", func(t *testing.T) {
		c := newFakeClock()
		q := NewLocalWithLease(30*time.Second, c.Now)
		if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := q.Claim(ctx, "w1"); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		c.Advance(31 * time.Second)
		if err := q.Ack(ctx, "r1"); !errors.Is(err, ErrNotHeld) {
			t.Errorf("Ack = %v, want ErrNotHeld", err)
		}
	})

	t.Run("ack twice", func(t *testing.T) {
		q := NewLocalWithLease(30*time.Second, clock.Now)
		if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := q.Claim(ctx, "w1"); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := q.Ack(ctx, "r1"); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if err := q.Ack(ctx, "r1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("second Ack = %v, want ErrNotFound", err)
		}
	})
}

func TestEnqueueDuplicate(t *testing.T) {
	q := NewLocal()
	ctx := context.Background()
	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(ctx, Run{ID: "r1"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate Enqueue = %v, want ErrDuplicate", err)
	}

	// Still a duplicate while the run is claimed.
	if _, err := q.Claim(ctx, "w1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Enqueue(ctx, Run{ID: "r1"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("Enqueue while claimed = %v, want ErrDuplicate", err)
	}

	// After Ack the ID is free again.
	if err := q.Ack(ctx, "r1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Errorf("Enqueue after Ack = %v, want nil", err)
	}
}

func TestClose(t *testing.T) {
	q := NewLocal()
	ctx := context.Background()
	if err := q.Enqueue(ctx, Run{ID: "r1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Claim(ctx, "w1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := q.Enqueue(ctx, Run{ID: "r2"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Enqueue after Close = %v, want ErrClosed", err)
	}
	if got, err := q.Claim(ctx, "w1"); !errors.Is(err, ErrClosed) || got != nil {
		t.Errorf("Claim after Close = %+v, %v; want nil, ErrClosed", got, err)
	}
	if err := q.Ack(ctx, "r1"); !errors.Is(err, ErrClosed) {
		t.Errorf("Ack after Close = %v, want ErrClosed", err)
	}
	if err := q.Heartbeat(ctx, "r1", "w1"); !errors.Is(err, ErrClosed) {
		t.Errorf("Heartbeat after Close = %v, want ErrClosed", err)
	}
	if err := q.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	q := NewLocal()
	ctx := context.Background()
	const workers = 8
	const runsPerWorker = 25

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			workerID := fmt.Sprintf("w%d", w)
			for i := 0; i < runsPerWorker; i++ {
				_ = q.Enqueue(ctx, Run{ID: fmt.Sprintf("%s-r%d", workerID, i)})
				claimed, err := q.Claim(ctx, workerID)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if claimed == nil {
					continue
				}
				_ = q.Heartbeat(ctx, claimed.Run.ID, workerID)
				if err := q.Ack(ctx, claimed.Run.ID); err != nil {
					t.Errorf("Ack(%s): %v", claimed.Run.ID, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
