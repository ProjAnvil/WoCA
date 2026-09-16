package relayclient

import (
	"io"
	"sync"
	"time"
)

// heartbeatInterval is the idle window after which an SSE comment ping is
// injected (design §6.3: 20s — intermediary idle windows run 60-100s).
const heartbeatInterval = 20 * time.Second

// pingLine is a valid SSE comment frame; single-line writes cannot tear
// data frames (design §6.3: every woca SSE frame is one Write and the
// pipe serializes writers).
const pingLine = ": ping\n\n"

// heartbeatWriter wraps the respond pipe for text/event-stream responses:
// the configured idle window injects one ping line (design §6.3 —
// MANDATORY, not optional; without it idle SSE dies in the relay front's
// proxy window). It also keeps the bridge→relay respond upload connection
// alive.
//
// mu guards ONLY the timer/done state — it is never held across the
// underlying write. A pipe write blocks until the respond upload starts
// reading, and execute calls arm exactly in that window (after rec.ready,
// before respond): holding mu across the write would deadlock arm against
// the handler's first frame. Frame safety does not need mu: io.Pipe
// serializes concurrent writers and delivers each Write contiguously, so
// a single-line ping can never tear a data frame (design §6.3).
type heartbeatWriter struct {
	w    io.Writer
	ivl  time.Duration
	done chan struct{}

	mu    sync.Mutex
	timer *time.Timer
}

func newHeartbeatWriter(w io.Writer, ivl time.Duration) *heartbeatWriter {
	return &heartbeatWriter{w: w, ivl: ivl, done: make(chan struct{})}
}

// arm starts the idle timer; unarmed writers pass data through untouched
// (non-SSE responses never inject pings).
func (h *heartbeatWriter) arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.timer != nil {
		return
	}
	h.timer = time.AfterFunc(h.ivl, h.ping)
}

func (h *heartbeatWriter) ping() {
	h.mu.Lock()
	select {
	case <-h.done:
		h.mu.Unlock()
		return
	default:
	}
	h.mu.Unlock()
	// A blocked pipe write is tolerated (design §6.3): the execute
	// cleanup closes the pipe on cancellation, which errors this write
	// and stops re-arming.
	if _, err := h.w.Write([]byte(pingLine)); err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return
	default:
	}
	if h.timer != nil {
		h.timer.Reset(h.ivl)
	}
}

// Write passes data through, resetting the idle timer.
func (h *heartbeatWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.timer != nil {
		h.timer.Reset(h.ivl)
	}
	return n, err
}

// stop ends the heartbeat; idempotent (execute defers it on every path).
func (h *heartbeatWriter) stop() {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	h.mu.Unlock()
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}
