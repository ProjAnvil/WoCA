package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ParkCapacity is the per-key parked-request ceiling (design §6.2: 64;
// a full queue answers 503).
const ParkCapacity = 64

var (
	// ErrQueueFull rejects a park beyond the per-key capacity.
	ErrQueueFull = errors.New("relay: parking queue full")
	// ErrTakeTimeout ends a long poll with no parked request (204).
	ErrTakeTimeout = errors.New("relay: park take timed out")
	// ErrUnknownParkedID rejects a respond for an id this key+generation
	// does not hold (404 — cross-key / cross-generation respond forgery
	// is impossible, design §6.1).
	ErrUnknownParkedID = errors.New("relay: unknown parked request")
	// ErrClientGone marks a respond for a request whose client already
	// disconnected (410 — the bridge unsticks its handler goroutine).
	ErrClientGone = errors.New("relay: client disconnected")
	// ErrRevoked drains the queue on key revocation (design §5.3) and
	// wakes in-flight polls (design §5.1).
	ErrRevoked = errors.New("relay: key revoked")
)

// ParkedRequest is one client request waiting for its bridge.
type ParkedRequest struct {
	ID       string
	Method   string
	Path     string
	Query    string
	ClientIP string
	Headers  map[string][]string
	Body     []byte
}

// ParkedResponse is the bridge's streamed answer handed to the parked
// client: status + headers freeze first, the body streams chunk by chunk.
type ParkedResponse struct {
	Status  int
	Headers http.Header
	Body    io.ReadCloser
}

type parkResult struct {
	resp *ParkedResponse
	err  error
}

type parkedItem struct {
	req        ParkedRequest
	result     chan parkResult
	failCause  chan error // buffered 1: revocation cause for the stream consumer
	streamDone chan struct{}
	// closeOnce makes every streamDone close idempotent: done() and FailAll
	// can race (revocation during streaming), and a check-then-close would
	// panic with "close of closed channel".
	closeOnce  sync.Once
	inFlight   bool
	generation string
	voided     bool
	completed  bool // set under q.mu: duplicate Complete is a no-op
	resp       *ParkedResponse
}

// closeStream closes streamDone exactly once, whoever calls it.
func (it *parkedItem) closeStream() {
	it.closeOnce.Do(func() { close(it.streamDone) })
}

// ParkingQueue holds per-key parked client requests (design §6.2): a
// client Park blocks until the bridge's respond completes it; a bridge
// Take long-polls for work.
type ParkingQueue struct {
	mu       sync.Mutex
	capacity int
	byKey    map[int64][]*parkedItem
	changed  chan struct{}
}

// NewParkingQueue builds a queue with the given per-key capacity.
func NewParkingQueue(capacity int) *ParkingQueue {
	return &ParkingQueue{capacity: capacity, byKey: map[int64][]*parkedItem{}, changed: make(chan struct{})}
}

// broadcastLocked wakes every Take waiter.
func (q *ParkingQueue) broadcastLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Park enqueues req and blocks until the bridge answers, the request
// context ends (client disconnect), or the queue is drained (revocation).
// The returned done func MUST be called when the response stream has been
// fully consumed — it releases the bridge's respond handler. failCause
// delivers the revocation cause to the stream consumer (the SSE error
// injection keys off it — deterministic, independent of read-error
// flavor).
func (q *ParkingQueue) Park(ctx context.Context, keyID int64, req ParkedRequest) (*ParkedResponse, func(), <-chan error, error) {
	item := &parkedItem{
		req:        req,
		result:     make(chan parkResult, 1),
		failCause:  make(chan error, 1),
		streamDone: make(chan struct{}),
	}
	q.mu.Lock()
	if len(q.byKey[keyID]) >= q.capacity {
		q.mu.Unlock()
		return nil, nil, nil, ErrQueueFull
	}
	q.byKey[keyID] = append(q.byKey[keyID], item)
	q.broadcastLocked()
	q.mu.Unlock()

	done := func() {
		q.mu.Lock()
		q.removeLocked(keyID, item)
		q.mu.Unlock()
		item.closeStream()
	}

	select {
	case res := <-item.result:
		if res.err != nil {
			done()
			return nil, nil, nil, res.err
		}
		return res.resp, done, item.failCause, nil
	case <-ctx.Done():
		q.mu.Lock()
		if item.inFlight {
			// The bridge already took it: keep the shell so its respond
			// gets ErrClientGone (410), not ErrUnknownParkedID.
			item.voided = true
			q.mu.Unlock()
		} else {
			q.removeLocked(keyID, item)
			q.mu.Unlock()
		}
		item.closeStream()
		return nil, nil, nil, ctx.Err()
	}
}

// Take long-polls for the next parked request of keyID, marking it
// in-flight for generation. ErrTakeTimeout ends the poll (204); a
// cancelled ctx (client hangup or revocation wakeup) returns ctx.Err().
func (q *ParkingQueue) Take(ctx context.Context, keyID int64, generation string, wait time.Duration) (*ParkedRequest, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		q.mu.Lock()
		for _, item := range q.byKey[keyID] {
			if !item.inFlight {
				item.inFlight = true
				item.generation = generation
				q.mu.Unlock()
				return &item.req, nil
			}
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrTakeTimeout
		case <-changed:
		}
	}
}

// Complete hands the bridge's respond to the parked client. The id must
// belong to this key and have been taken by this generation — anything
// else is ErrUnknownParkedID (404); a client that disconnected meanwhile
// yields ErrClientGone (410). The returned channel closes when the client
// side finishes streaming the body.
func (q *ParkingQueue) Complete(keyID int64, generation, id string, resp *ParkedResponse) (<-chan struct{}, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, item := range q.byKey[keyID] {
		if item.req.ID != id {
			continue
		}
		if !item.inFlight || item.generation != generation {
			return nil, ErrUnknownParkedID
		}
		if item.voided {
			q.removeLocked(keyID, item)
			return nil, ErrClientGone
		}
		if item.completed {
			// Duplicate respond for an already-completed id (bridge retry):
			// no-op — the first respond wins. Without this guard the send
			// below blocks on the full result channel while holding q.mu,
			// freezing every Park/Take/Len.
			return item.streamDone, nil
		}
		item.completed = true
		item.resp = resp
		item.result <- parkResult{resp: resp}
		return item.streamDone, nil
	}
	return nil, ErrUnknownParkedID
}

// FailAll drains every parked request of keyID with err (revocation,
// design §5.3). In-flight items additionally have their streamDone closed
// so the respond handler stops waiting, and their failCause carries err
// to the stream consumer.
func (q *ParkingQueue) FailAll(keyID int64, err error) {
	q.mu.Lock()
	items := q.byKey[keyID]
	delete(q.byKey, keyID)
	q.broadcastLocked()
	q.mu.Unlock()
	for _, item := range items {
		select {
		case item.failCause <- err:
		default:
		}
		select {
		case item.result <- parkResult{err: err}:
		default:
		}
		// Release the bridge's respond handler BEFORE closing the streamed
		// body. streamDone lets HandleRespond return, and net/http's
		// finishRequest then abortPendingRead — forcing the parked client's
		// in-flight body Read (which holds http.body's internal mutex) to
		// error out. Closing the body first deadlocks on that mutex: the
		// Read holds it while blocked on the bridge's socket, which only
		// the abort unsticks (Task 6 cascade verification).
		item.closeStream()
		if item.resp != nil && item.resp.Body != nil {
			_ = item.resp.Body.Close()
		}
	}
}

// Len reports the parked count for keyID (status + tests).
func (q *ParkingQueue) Len(keyID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.byKey[keyID])
}

func (q *ParkingQueue) removeLocked(keyID int64, item *parkedItem) {
	items := q.byKey[keyID]
	for i, it := range items {
		if it == item {
			q.byKey[keyID] = append(items[:i], items[i+1:]...)
			return
		}
	}
}
