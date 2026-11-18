package server

// Per-session finalizer queue for queued-cancel persistence (cancel-semantics
// gap 1, option 1a): a run cancelled while QUEUED persists a derived history
// (its prompt plus the stop notice), but writing it immediately lands
// OUTSIDE the admission serialization window — the in-flight holder's
// completion save is a full overwrite built from the history it loaded at
// run start, so the out-of-band write is clobbered. Instead the queued
// branch enqueues a finalizer here; the holder's completeRun drains the
// session's FIFO after its own save and before admission.release, restoring
// single-writer discipline per session without serializing execution (a
// cancelled run must not consume a promotion slot or block the next real
// run). Parallel sessions are unaffected: the map is per-session and no
// global lock is held across I/O — the drain runs in the holder's
// goroutine, not under admission.mu.

// enqueueCancelFinalizer appends fn to sid's FIFO — or executes it
// immediately when no serialization window exists to join:
//   - the session gate is idle (admission.busy is false: no holder will
//     ever drain the FIFO, and with no concurrent run there is nothing to
//     clobber the write), or
//   - the registry is closed (shutdown: the holder may never reach
//     completeRun before the process exits, so the finalizer runs
//     best-effort now — the same posture the pre-fix immediate save had,
//     bounded to the shutdown window).
func (s *Server) enqueueCancelFinalizer(sid string, fn func()) {
	s.cancelMu.Lock()
	if s.cancelFinalizersClosed || !s.admission.busy(sid) {
		s.cancelMu.Unlock()
		fn()
		return
	}
	s.pendingCancelFinalizers[sid] = append(s.pendingCancelFinalizers[sid], fn)
	s.cancelMu.Unlock()
}

// drainCancelFinalizers runs sid's pending finalizers in FIFO order. It is
// called from completeRun between the holder's own save and the admission
// release, so each deferred save re-loads the just-persisted history,
// appends its prompt + stop notice, and lands inside the holder's window —
// the next promoted run is guaranteed to see every write. Executing a
// finalizer re-enters completeRun (heldGate=false) and may drain again; the
// slice is swapped out under the lock before anything runs, so re-entrant
// drains find an empty FIFO and cannot recurse into themselves.
//
// Bounded-delay note: a finalizer pushed after the holder's drain popped
// the slice but before its release (a cancel landing in that window) sits
// in the FIFO until the session's NEXT run completes or shutdown flushes —
// it is never lost, and the next holder's drain persists it in order.
func (s *Server) drainCancelFinalizers(sid string) {
	s.cancelMu.Lock()
	fns := s.pendingCancelFinalizers[sid]
	delete(s.pendingCancelFinalizers, sid)
	s.cancelMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// flushCancelFinalizers is the shutdown drain: after admission.stop()
// aborted the queued waiters, run every pending finalizer best-effort and
// close the registry so late pushes (waiters that had not reached their
// branch yet) execute immediately instead of waiting for a holder that may
// never complete. In-flight runs are not cancelled at shutdown, so a holder
// still executing may later clobber these writes — accepted best-effort,
// identical to the pre-fix posture but confined to the shutdown window.
func (s *Server) flushCancelFinalizers() {
	s.cancelMu.Lock()
	s.cancelFinalizersClosed = true
	pending := s.pendingCancelFinalizers
	s.pendingCancelFinalizers = map[string][]func(){}
	s.cancelMu.Unlock()
	for _, fns := range pending {
		for _, fn := range fns {
			fn()
		}
	}
}
