package server

import (
	"context"
	"sync"
)

// maxQueuedPerSession bounds the FIFO of runs waiting on one session gate;
// POSTs beyond it are rejected with 409 (runaway-client guard).
const maxQueuedPerSession = 8

// admission serializes runs per session: at most one run holds a session's
// gate, later runs queue FIFO, and a queued run is promoted only after the
// holder's transcript has been persisted (release happens after onDone
// returns — see startRun). This eliminates the overlapping-run overwrite
// that dropped the earlier run's reply.
type admission struct {
	mu       sync.Mutex
	gates    map[string]*sessionGate
	stopped  bool
	stopCh   chan struct{}
	stopOnce func()
}

type sessionGate struct {
	running bool
	queue   []*gateWaiter
}

type gateWaiter struct {
	ready      chan struct{}
	onPosition func(int) // 1-based queue position updates; must be non-blocking
}

func newAdmission() *admission {
	a := &admission{gates: map[string]*sessionGate{}, stopCh: make(chan struct{})}
	a.stopOnce = sync.OnceFunc(func() {
		a.mu.Lock()
		a.stopped = true
		a.mu.Unlock()
		close(a.stopCh)
	})
	return a
}

// enter takes or queues for the session gate. pos=0 means granted (w==nil);
// pos>=1 means queued (wait on w). ok=false means the queue is full or
// admission is stopped. A successful caller must eventually call release —
// for queued waiters only after wait returned true.
func (a *admission) enter(sessionID string, onPosition func(int)) (*gateWaiter, int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil, -1, false
	}
	g := a.gates[sessionID]
	if g == nil {
		g = &sessionGate{}
		a.gates[sessionID] = g
	}
	if !g.running && len(g.queue) == 0 {
		g.running = true
		return nil, 0, true
	}
	if len(g.queue) >= maxQueuedPerSession {
		return nil, -1, false
	}
	w := &gateWaiter{ready: make(chan struct{}), onPosition: onPosition}
	g.queue = append(g.queue, w)
	return w, len(g.queue), true
}

// wait blocks until the waiter is promoted (true) or removed due to ctx
// cancellation / admission stop (false).
func (a *admission) wait(ctx context.Context, sessionID string, w *gateWaiter) bool {
	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
	case <-a.stopCh:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gates[sessionID]
	if g == nil {
		return false
	}
	select {
	case <-w.ready:
		return true // promoted while ctx fired; caller's ctx is done, run exits fast
	default:
	}
	if removeWaiter(g, w) {
		renumberLocked(g)
		if !g.running && len(g.queue) == 0 {
			delete(a.gates, sessionID)
		}
	}
	return false
}

// release frees the gate and promotes the FIFO head, renumbering the rest.
func (a *admission) release(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gates[sessionID]
	if g == nil {
		return
	}
	if len(g.queue) == 0 {
		g.running = false
		delete(a.gates, sessionID) // keep the map bounded
		return
	}
	head := g.queue[0]
	g.queue = g.queue[1:]
	close(head.ready)
	renumberLocked(g)
}

func removeWaiter(g *sessionGate, w *gateWaiter) bool {
	for i, q := range g.queue {
		if q == w {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			return true
		}
	}
	return false
}

// renumberLocked reports 1-based positions to remaining waiters. Callbacks
// must be non-blocking (Run.emit is: its fanout drops on full channels).
func renumberLocked(g *sessionGate) {
	for i, w := range g.queue {
		if w.onPosition != nil {
			w.onPosition(i + 1)
		}
	}
}

// stop aborts all queued waiters (server shutdown); held gates are
// untouched — in-flight runs finish through the existing cancel path.
func (a *admission) stop() {
	a.stopOnce()
}

// stats reports total queued waiters and per-session depths (for /api/stats).
func (a *admission) stats() (int, map[string]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	depths := map[string]int{}
	total := 0
	for sid, g := range a.gates {
		if len(g.queue) > 0 {
			depths[sid] = len(g.queue)
			total += len(g.queue)
		}
	}
	return total, depths
}
