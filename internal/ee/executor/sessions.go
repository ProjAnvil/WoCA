package executor

// In-memory session state for the sandbox HTTP service: started processes
// (contract v2 §3.3-3.6) live as procSession entries inside their sandbox.
// Each session runs a pump goroutine that drains its Process into a bounded
// replay buffer — a flooding process cannot OOM the executor — and a
// service-wide reaper reclaims sessions nobody has touched for the idle
// TTL. The registry is deliberately pure memory: an executor restart kills
// every session, which is exactly the documented failure semantics (design
// §3 rule 5, §10.2).

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// sessionOutputCap bounds one session's retained output events at 8 MiB
// (design §3 rule 4): output past the cap is truncated from the front, the
// offset floor rises with it, and next_offset keeps advancing so clients
// that poll promptly never lose data. Package variable as a test seam.
var sessionOutputCap = 8 << 20

// sessionIdleTTL is how long an untouched session may sit around before the
// reaper reclaims it (contract §5's server-side TTL safety net). Test seam.
var sessionIdleTTL = 30 * time.Minute

// sessionPumpInterval is how often a session pump polls its Process for new
// output.
var sessionPumpInterval = 20 * time.Millisecond

// procSession is one started process's service-side state: the bounded
// event buffer plus bookkeeping. The live connection semantics — stdin
// writes, snapshots, kill — are carried by the Process handle; the session
// only mirrors what has been drained so far.
type procSession struct {
	proc Process

	mu       sync.Mutex
	events   []OutputEvent // retained events, oldest first
	total    int           // cumulative bytes ever produced (the next_offset cursor)
	truncLen int           // offset floor: everything below this was truncated away
	done     bool
	exitCode int
	lastUsed time.Time
	closed   bool
}

func newProcSession(proc Process) *procSession {
	return &procSession{proc: proc, lastUsed: time.Now()}
}

// pump drains the process output into the bounded buffer until the process
// finishes (Snapshot reports done only once all output is drained) or ctx
// is canceled. It is the only writer of total/events after registration.
func (ses *procSession) pump(ctx context.Context) {
	t := time.NewTicker(sessionPumpInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ses.mu.Lock()
		offset := ses.total
		ses.mu.Unlock()

		events, next, done, exit := ses.proc.Snapshot(offset)

		ses.mu.Lock()
		if next > ses.total {
			ses.total = next
		}
		ses.events = append(ses.events, events...)
		ses.truncateLocked()
		if done {
			ses.done = true
			ses.exitCode = exit
			ses.mu.Unlock()
			return
		}
		ses.mu.Unlock()
	}
}

// truncateLocked drops oldest events until the retained payload fits the
// cap, always keeping the newest event (a single chunk larger than the cap
// is retained whole rather than silently vanishing), and raises the offset
// floor to the first retained event. next_offset (total) is untouched:
// the cursor is monotonic even though the history is not.
func (ses *procSession) truncateLocked() {
	sum := 0
	for _, e := range ses.events {
		sum += len(e.Data)
	}
	for sum > sessionOutputCap && len(ses.events) > 1 {
		sum -= len(ses.events[0].Data)
		ses.events = ses.events[1:]
	}
	if len(ses.events) > 0 {
		if floor := ses.events[0].Start; floor > ses.truncLen {
			ses.truncLen = floor
		}
	}
}

// snapshot returns the retained events at or after offset, the monotonic
// next offset, whether the process finished, and its exit code.
func (ses *procSession) snapshot(offset int) (events []OutputEvent, next int, done bool, exit int) {
	ses.mu.Lock()
	defer ses.mu.Unlock()
	events = make([]OutputEvent, 0, len(ses.events))
	for _, e := range ses.events {
		if e.Start >= offset {
			events = append(events, e)
		}
	}
	return events, ses.total, ses.done, ses.exitCode
}

// isDone reports whether the process has finished (per the last pump
// observation).
func (ses *procSession) isDone() bool {
	ses.mu.Lock()
	defer ses.mu.Unlock()
	return ses.done
}

// touch marks the session as in use, resetting the idle TTL clock.
func (ses *procSession) touch() {
	ses.mu.Lock()
	defer ses.mu.Unlock()
	ses.lastUsed = time.Now()
}

// idleFor reports how long the session went untouched.
func (ses *procSession) idleFor(now time.Time) time.Duration {
	ses.mu.Lock()
	defer ses.mu.Unlock()
	return now.Sub(ses.lastUsed)
}

// close stops the process (stdin closed, killed if running, output drained)
// and marks the session closed; idempotent. The buffered events stay
// readable so a final output poll can still report the exit code until the
// holder drops the session.
func (ses *procSession) close() {
	ses.mu.Lock()
	if ses.closed {
		ses.mu.Unlock()
		return
	}
	ses.closed = true
	ses.mu.Unlock()
	_ = ses.proc.Close()
}

// sandbox is one live sandbox served by the service: its container, the
// workspace lock held from create to destroy, and its in-memory sessions.
type sandbox struct {
	id        string
	container Container
	workspace string // workspace id; "" when not workspace-routed
	lock      *DirLock

	mu       sync.Mutex
	sessions map[string]*procSession
	nextSes  int64
}

// addSession registers a started process and returns its session id.
func (sbx *sandbox) addSession(proc Process) (string, *procSession) {
	sbx.mu.Lock()
	defer sbx.mu.Unlock()
	sbx.nextSes++
	id := "ses_" + strconv.FormatInt(sbx.nextSes, 10)
	if sbx.sessions == nil {
		sbx.sessions = map[string]*procSession{}
	}
	ses := newProcSession(proc)
	sbx.sessions[id] = ses
	return id, ses
}

// session looks up one session, or nil.
func (sbx *sandbox) session(id string) *procSession {
	sbx.mu.Lock()
	defer sbx.mu.Unlock()
	return sbx.sessions[id]
}

// reapIdle closes and forgets sessions idle beyond ttl.
func (sbx *sandbox) reapIdle(now time.Time, ttl time.Duration) {
	sbx.mu.Lock()
	var stale []*procSession
	for id, ses := range sbx.sessions {
		if ses.idleFor(now) > ttl {
			stale = append(stale, ses)
			delete(sbx.sessions, id)
		}
	}
	sbx.mu.Unlock()
	for _, ses := range stale {
		ses.close()
	}
}

// closeAllSessions stops every session of the sandbox.
func (sbx *sandbox) closeAllSessions() {
	sbx.mu.Lock()
	sessions := slices.Collect(maps.Values(sbx.sessions))
	sbx.sessions = map[string]*procSession{}
	sbx.mu.Unlock()
	for _, ses := range sessions {
		ses.close()
	}
}

// destroy is the teardown shared by the destroy endpoint and service Close:
// kill every session, force-remove the container (idempotent at the backend
// level), release the workspace lock. Errors are returned for the caller to
// surface; lock-release failures are logged (the kernel releases a flock on
// close anyway, so they cannot wedge a workspace).
func (sbx *sandbox) destroy(ctx context.Context, log zerolog.Logger) error {
	sbx.closeAllSessions()
	err := sbx.container.Remove(ctx, true)
	if err != nil && !isRemovedErr(err) {
		err = fmt.Errorf("destroy sandbox %s: %w", sbx.id, err)
	} else {
		err = nil
	}
	if sbx.lock != nil {
		if lerr := sbx.lock.Close(); lerr != nil {
			log.Error().Err(lerr).Str("sandbox", sbx.id).Msg("workspace lock release failed")
		}
	}
	return err
}
