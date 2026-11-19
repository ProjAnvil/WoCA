package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
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
// deployments with the database disabled and tests. Every method takes the
// caller's context: on postgres the database backend scopes its writes and
// reads to the context's tenant (the session_state table sits in the FORCE
// RLS loop), so the busy rows a tenant's run produces are written and read
// under that tenant instead of the 'local' default.
type sessionStateStore interface {
	Set(ctx context.Context, st wocadb.SessionState)
	// ClearIfRun removes the row only while it still points at runID
	// (compare-and-clear; a finished run must not clear the row of the run
	// promoted behind it). It reports whether the row was removed.
	ClearIfRun(ctx context.Context, sessionID, runID string) bool
	Delete(ctx context.Context, sessionID string)
	Snapshot(ctx context.Context) map[string]wocadb.SessionState
}

// dbSessionState stores run state in the woca database.
type dbSessionState struct{ db *wocadb.DB }

func (s *dbSessionState) Set(ctx context.Context, st wocadb.SessionState) {
	if err := s.db.SetSessionState(ctx, st); err != nil {
		// State marks are best-effort durability: a failed write degrades to
		// a stale row (cleared at the next reconcile), never a failed run.
		return
	}
}

func (s *dbSessionState) ClearIfRun(ctx context.Context, sessionID, runID string) bool {
	ok, err := s.db.ClearSessionState(ctx, sessionID, runID)
	return ok && err == nil
}

func (s *dbSessionState) Delete(ctx context.Context, sessionID string) {
	_ = s.db.DeleteSessionState(ctx, sessionID)
}

func (s *dbSessionState) Snapshot(ctx context.Context) map[string]wocadb.SessionState {
	rows, err := s.db.SessionStates(ctx)
	if err != nil {
		return map[string]wocadb.SessionState{}
	}
	out := make(map[string]wocadb.SessionState, len(rows))
	for _, r := range rows {
		out[r.SessionID] = r
	}
	return out
}

// memSessionState is the no-database fallback (this process only); the
// context is accepted for interface parity and ignored.
type memSessionState struct {
	mu   sync.Mutex
	rows map[string]wocadb.SessionState
}

func newMemSessionState() *memSessionState {
	return &memSessionState{rows: map[string]wocadb.SessionState{}}
}

func (s *memSessionState) Set(_ context.Context, st wocadb.SessionState) {
	s.mu.Lock()
	s.rows[st.SessionID] = st
	s.mu.Unlock()
}

func (s *memSessionState) ClearIfRun(_ context.Context, sessionID, runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.rows[sessionID]; ok && cur.RunID == runID {
		delete(s.rows, sessionID)
		return true
	}
	return false
}

func (s *memSessionState) Delete(_ context.Context, sessionID string) {
	s.mu.Lock()
	delete(s.rows, sessionID)
	s.mu.Unlock()
}

func (s *memSessionState) Snapshot(_ context.Context) map[string]wocadb.SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]wocadb.SessionState, len(s.rows))
	maps.Copy(out, s.rows)
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
// The reconcile reads tenantless, so on postgres it sees the 'local' rows
// only: tenant-tagged rows are invisible to the owner connection without
// the tenant GUC (FORCE RLS), and this process cannot enumerate other
// tenants' identities — their stale rows age out with the run ledger's own
// crash recovery instead.
func (s *Server) reconcileStaleSessionState() {
	host := ownerHost(s.owner)
	for sid, row := range s.state.Snapshot(context.Background()) {
		pid, rowHost := parseOwner(row.Owner)
		if rowHost != host || pid == 0 || processAlive(pid) {
			continue
		}
		s.state.Delete(context.Background(), sid)
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

// sessionStateTopic is the eventbus topic carrying session state changes to
// the global SSE stream. The hub's subscriptions are all live-only; the
// bounded history retention is only a truncation buffer against slow
// consumers.
const sessionStateTopic = "session-state"

// markSessionState records a session's run state durably and broadcasts it.
// ctx scopes the database write to the run's tenant (the caller passes the
// run's own context — request-derived for gateway runs, restored-from-claim
// for dispatched ones); the broadcast stays process-local and unscoped.
func (s *Server) markSessionState(ctx context.Context, sessionID, status, runID string) {
	s.state.Set(ctx, wocadb.SessionState{
		SessionID: sessionID,
		Status:    status,
		RunID:     runID,
		Owner:     s.owner,
		UpdatedAt: wocadb.Now(),
	})
	// The local bus never fails; a dropped event on a slow client is
	// recovered by the seeded snapshot on reconnect, as before. Still, log
	// a failure instead of swallowing it silently.
	if _, err := s.hub.Publish(context.Background(), sessionStateTopic, s.sessionStateEvent(sessionID, status, runID)); err != nil {
		s.log.Debug().Err(err).Str("session", sessionID).Msg("session-state publish failed")
	}
}

// clearSessionState removes the state row when it still belongs to runID.
// ctx scopes the compare-and-clear to the run's tenant.
func (s *Server) clearSessionState(ctx context.Context, sessionID, runID string) {
	if s.state.ClearIfRun(ctx, sessionID, runID) {
		if _, err := s.hub.Publish(context.Background(), sessionStateTopic, s.sessionStateEvent(sessionID, "", runID)); err != nil {
			s.log.Debug().Err(err).Str("session", sessionID).Msg("session-state publish failed")
		}
	}
}

// forgetSessionState drops the row unconditionally (session deleted). ctx
// scopes the delete to the deleting request's tenant.
func (s *Server) forgetSessionState(ctx context.Context, sessionID string) {
	s.state.Delete(ctx, sessionID)
	if _, err := s.hub.Publish(context.Background(), sessionStateTopic, s.sessionStateEvent(sessionID, "", "")); err != nil {
		s.log.Debug().Err(err).Str("session", sessionID).Msg("session-state publish failed")
	}
}

func (s *Server) sessionStateEvent(sessionID, status, runID string) Event {
	return Event{Type: EventSessionState, Data: map[string]any{
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher := http.NewResponseController(w)
	if err := flusher.Flush(); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sub, err := s.hub.Subscribe(r.Context(), sessionStateTopic, -1) // live-only
	if err != nil {
		http.Error(w, "event bus unavailable", http.StatusInternalServerError)
		return
	}
	defer sub.Unsubscribe()

	// Seed with the snapshot (oldest first for deterministic output). The
	// request context scopes the read to the connecting client's tenant on
	// postgres, so each tenant's global stream starts from exactly its own
	// session states.
	states := s.state.Snapshot(r.Context())
	rows := make([]wocadb.SessionState, 0, len(states))
	for _, st := range states {
		rows = append(rows, st)
	}
	slices.SortFunc(rows, func(a, b wocadb.SessionState) int { return cmp.Compare(a.UpdatedAt, b.UpdatedAt) })
	for _, st := range rows {
		writeSSE(w, s.sessionStateEvent(st.SessionID, st.Status, st.RunID))
	}
	// Always emit an initial frame: the relay tunnel's streamRecorder
	// commits response headers at the first Write (design §6.3), so an
	// empty snapshot must still write — an SSE comment is a no-op for
	// EventSource but establishes the stream (and arms the heartbeat).
	fmt.Fprint(w, ": connected\n\n")
	_ = flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				return // bus closed; never write a zero-value event
			}
			writeSSE(w, ev)
			_ = flusher.Flush()
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
