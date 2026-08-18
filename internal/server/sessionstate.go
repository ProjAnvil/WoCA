package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/projanvil/woca/internal/wocadb"
)

// Session run state: which sessions are busy, recorded durably so listings
// (and other processes sharing the database) can show it. A row exists only
// while a run is in flight — status running (actively executing) or awaiting
// (parked on an unanswered interrupt). The run's own SSE stream remains the
// fine-grained channel; this is the coarse per-session signal the session
// list and the global event stream key off.

// sessionStateStore persists per-session run state. The database backend is
// the real one (woca.db session_state table); an in-memory map backs
// deployments with the database disabled and tests.
type sessionStateStore interface {
	Set(st wocadb.SessionState)
	// ClearIfRun removes the row only while it still points at runID
	// (compare-and-clear; a finished run must not clear the row of the run
	// promoted behind it). It reports whether the row was removed.
	ClearIfRun(sessionID, runID string) bool
	Delete(sessionID string)
	Snapshot() map[string]wocadb.SessionState
}

// dbSessionState stores run state in the woca database.
type dbSessionState struct{ db *wocadb.DB }

func (s *dbSessionState) Set(st wocadb.SessionState) {
	if err := s.db.SetSessionState(st); err != nil {
		// State marks are best-effort durability: a failed write degrades to
		// a stale row (cleared at the next reconcile), never a failed run.
		return
	}
}

func (s *dbSessionState) ClearIfRun(sessionID, runID string) bool {
	ok, err := s.db.ClearSessionState(sessionID, runID)
	return ok && err == nil
}

func (s *dbSessionState) Delete(sessionID string) {
	_ = s.db.DeleteSessionState(sessionID)
}

func (s *dbSessionState) Snapshot() map[string]wocadb.SessionState {
	rows, err := s.db.SessionStates()
	if err != nil {
		return map[string]wocadb.SessionState{}
	}
	out := make(map[string]wocadb.SessionState, len(rows))
	for _, r := range rows {
		out[r.SessionID] = r
	}
	return out
}

// memSessionState is the no-database fallback (this process only).
type memSessionState struct {
	mu   sync.Mutex
	rows map[string]wocadb.SessionState
}

func newMemSessionState() *memSessionState {
	return &memSessionState{rows: map[string]wocadb.SessionState{}}
}

func (s *memSessionState) Set(st wocadb.SessionState) {
	s.mu.Lock()
	s.rows[st.SessionID] = st
	s.mu.Unlock()
}

func (s *memSessionState) ClearIfRun(sessionID, runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.rows[sessionID]; ok && cur.RunID == runID {
		delete(s.rows, sessionID)
		return true
	}
	return false
}

func (s *memSessionState) Delete(sessionID string) {
	s.mu.Lock()
	delete(s.rows, sessionID)
	s.mu.Unlock()
}

func (s *memSessionState) Snapshot() map[string]wocadb.SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]wocadb.SessionState, len(s.rows))
	for k, v := range s.rows {
		out[k] = v
	}
	return out
}

// newStateOwner stamps state rows with the recording process ("pid@host") so
// a restart can reconcile rows whose process died between mark and clear.
func newStateOwner() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%d@%s", os.Getpid(), host)
}

// reconcileStaleSessionState clears rows whose owning process no longer
// exists — the crash-between-mark-and-clear residue. Rows from other hosts
// (postgres deployments) are left alone: liveness is unknowable remotely.
func (s *Server) reconcileStaleSessionState() {
	host := ownerHost(s.owner)
	for sid, row := range s.state.Snapshot() {
		pid, rowHost := parseOwner(row.Owner)
		if rowHost != host || pid == 0 || processAlive(pid) {
			continue
		}
		s.state.Delete(sid)
	}
}

func ownerHost(owner string) string {
	_, host := parseOwner(owner)
	return host
}

func parseOwner(owner string) (pid int, host string) {
	pidStr, host, ok := strings.Cut(owner, "@")
	if !ok {
		return 0, ""
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, host
	}
	return pid, host
}

// processAlive reports whether pid is running (signal 0 probe; EPERM means
// alive under another user).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// eventHub fans out server-level events (session state changes) to all
// connected global SSE clients. No history retention: state events are
// idempotent snapshots, and a fresh connection is seeded with the current
// state, so replay would only be redundant.
type eventHub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[*subscriber]struct{}{}}
}

func (h *eventHub) subscribe() *subscriber {
	sub := &subscriber{ch: make(chan Event, 256)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *eventHub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// emit fans out non-blockingly: a slow global-events client must never stall
// run machinery. A dropped state event leaves the client stale until its next
// change or reconnect (EventSource auto-reconnects; the seeded snapshot
// resyncs it).
func (h *eventHub) emit(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// markSessionState records a session's run state durably and broadcasts it.
func (s *Server) markSessionState(sessionID, status, runID string) {
	s.state.Set(wocadb.SessionState{
		SessionID: sessionID,
		Status:    status,
		RunID:     runID,
		Owner:     s.owner,
		UpdatedAt: wocadb.Now(),
	})
	s.hub.emit(s.sessionStateEvent(sessionID, status, runID))
}

// clearSessionState removes the state row when it still belongs to runID.
func (s *Server) clearSessionState(sessionID, runID string) {
	if s.state.ClearIfRun(sessionID, runID) {
		s.hub.emit(s.sessionStateEvent(sessionID, "", runID))
	}
}

// forgetSessionState drops the row unconditionally (session deleted).
func (s *Server) forgetSessionState(sessionID string) {
	s.state.Delete(sessionID)
	s.hub.emit(s.sessionStateEvent(sessionID, "", ""))
}

func (s *Server) sessionStateEvent(sessionID, status, runID string) Event {
	return Event{Type: "session_state", Data: map[string]any{
		"session_id": sessionID,
		"status":     status, // "" = idle (row gone)
		"run_id":     runID,
	}}
}

// handleGlobalEvents serves GET /api/events: a server-level SSE stream of
// session state changes (running / awaiting / idle), open to any client —
// including tabs that never started a run and pages restored after a
// refresh. Each connection is seeded with the current state snapshot, so
// connecting is itself a resync.
func (s *Server) handleGlobalEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	// Seed with the snapshot (oldest first for deterministic output).
	states := s.state.Snapshot()
	rows := make([]wocadb.SessionState, 0, len(states))
	for _, st := range states {
		rows = append(rows, st)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt < rows[j].UpdatedAt })
	for _, st := range rows {
		writeSSE(w, s.sessionStateEvent(st.SessionID, st.Status, st.RunID))
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case ev := <-sub.ch:
			writeSSE(w, ev)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// writeSSE frames one event (no id line: global events carry no replay seq).
func writeSSE(w http.ResponseWriter, ev Event) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
}
