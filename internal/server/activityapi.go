package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/tools"
)

// Session activity API (PM §4.3): the panel's read surface is one snapshot
// plus per-item output tails, its write surface is user-initiated stops
// (kill shell, stop task, delete cron). All facts come from the agent's
// activity seam (internal/agent/activity.go) — ownership included: the
// server never re-filters ids, the seam's ErrActivityNotFound (unknown or
// foreign) maps to 404, and changes are announced as signal-only
// activity_changed events on the global stream (PM §4.4, D4).

// requireActivityAPI gates every activity handler: the deployment form must
// have assembled the surface (Config.ActivityAPI, set in servefront —
// never sniffed from the request) and there must be an agent to ask. Both
// unsupported postures answer 501 S-B-0501 so the frontend can render
// "unavailable in this deployment form" instead of empty lists.
func (s *Server) requireActivityAPI(w http.ResponseWriter) bool {
	if s.activityAPI && s.agent != nil {
		return true
	}
	httpx.WriteCodedMsg(w, http.StatusNotImplemented, httpx.SegBusiness, httpx.CodeNotImplemented,
		"session activity API is not available in this deployment form")
	return false
}

func activityNotFound(w http.ResponseWriter) {
	httpx.WriteCodedMsg(w, http.StatusNotFound, httpx.SegBusiness, httpx.CodeNotFound,
		"no such background item in this session")
}

func activityBadTailBytes(w http.ResponseWriter) {
	httpx.WriteCodedMsg(w, http.StatusBadRequest, httpx.SegBusiness, httpx.CodeInvalidRequest,
		"tail_bytes must be a non-negative integer")
}

func activityInternal(w http.ResponseWriter) {
	httpx.WriteCodedMsg(w, http.StatusInternalServerError, httpx.SegBusiness, httpx.CodeInternal,
		"activity lookup failed")
}

// rfc3339UTC renders t per the panel contract (PM §4.3): RFC3339, UTC.
func rfc3339UTC(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// rfc3339UTCOrNull renders a nullable time: zero reads as null, the panel's
// "not yet / never" marker.
func rfc3339UTCOrNull(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := rfc3339UTC(t)
	return &s
}

// tailBytesParam parses ?tail_bytes=N: absent means the seam default,
// values above the cap are clamped by the seam; anything that is not a
// non-negative integer is a 400, never a guess.
func tailBytesParam(r *http.Request) (int, bool) {
	v := r.URL.Query().Get("tail_bytes")
	if v == "" {
		return agent.ActivityTailBytesDefault, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Snapshot and item DTOs. Fields are declared in JSON-key alphabetical
// order (file convention) and every array is non-nil so JSON renders []
// instead of null (PM §4.3).

type activityTodos struct {
	Items     []tools.Todo `json:"items"`
	UpdatedAt *string      `json:"updated_at"` // null: the session never wrote todos
}

type activityShell struct {
	Command     string  `json:"command"`
	EndedAt     *string `json:"ended_at"`
	ExitCode    *int    `json:"exit_code"` // null while running or after a signal kill
	ID          string  `json:"id"`
	OutputBytes int     `json:"output_bytes"`
	StartedAt   string  `json:"started_at"`
	Status      string  `json:"status"`
	StoppedBy   string  `json:"stopped_by"`
}

type activityTask struct {
	Description string  `json:"description"`
	EndedAt     *string `json:"ended_at"`
	ID          string  `json:"id"`
	StartedAt   string  `json:"started_at"`
	Status      string  `json:"status"`
	StoppedBy   string  `json:"stopped_by"`
	Subagent    string  `json:"subagent"`
}

type activityCron struct {
	CreatedAt   string  `json:"created_at"`
	ID          string  `json:"id"`
	LastFiredAt *string `json:"last_fired_at"`
	Name        string  `json:"name"`
	NextFireAt  *string `json:"next_fire_at"` // null: no fire within the scheduler's horizon (cached per entry, F5)
	Prompt      string  `json:"prompt"`
	Spec        string  `json:"spec"`
}

type activityWakeInfo struct {
	Capped               bool `json:"capped"`
	Enabled              bool `json:"enabled"`
	PendingNotifications int  `json:"pending_notifications"`
	Stalled              bool `json:"stalled"`
}

type activitySnapshot struct {
	Crons      []activityCron   `json:"crons"`
	ServerTime string           `json:"server_time"`
	SessionID  string           `json:"session_id"`
	Shells     []activityShell  `json:"shells"`
	Tasks      []activityTask   `json:"tasks"`
	Todos      activityTodos    `json:"todos"`
	Wake       activityWakeInfo `json:"wake"`
}

type activityOutput struct {
	ID         string `json:"id"`
	Output     string `json:"output"`
	Status     string `json:"status"`
	TotalBytes int    `json:"total_bytes"`
	Truncated  bool   `json:"truncated"`
}

type activityShellAction struct {
	AlreadyFinished bool          `json:"already_finished"`
	Shell           activityShell `json:"shell"`
}

type activityTaskAction struct {
	AlreadyFinished bool         `json:"already_finished"`
	Task            activityTask `json:"task"`
}

type activityCronDeleted struct {
	Deleted bool   `json:"deleted"`
	ID      string `json:"id"`
}

// shellStatusText maps the shell facts onto the panel's status vocabulary
// (PM §4.2). A kill wins over everything: Killed renders "stopped" with
// attribution even in the window where the process is still settling (and
// over a racing natural exit, Killed && ExitCode >= 0 — the seam's
// documented rule), so a kill response never says "running". An exit code
// below 0 only ever follows a signal, so it can never make a shell
// "failed".
func shellStatusText(st tools.BackgroundShellStatus) string {
	switch {
	case st.Killed:
		return "stopped"
	case st.Running:
		return "running"
	case st.ExitCode == 0:
		return "completed"
	default:
		return "failed"
	}
}

// shellExitCode reports the recorded exit code, or null while running and
// for the signal-killed (-1) case.
func shellExitCode(st tools.BackgroundShellStatus) *int {
	if st.Running || st.ExitCode < 0 {
		return nil
	}
	c := st.ExitCode
	return &c
}

func shellItem(st tools.BackgroundShellStatus) activityShell {
	return activityShell{
		Command:     st.Command,
		EndedAt:     rfc3339UTCOrNull(st.EndedAt),
		ExitCode:    shellExitCode(st),
		ID:          st.ID,
		OutputBytes: st.OutputBytes,
		StartedAt:   rfc3339UTC(st.StartedAt),
		Status:      shellStatusText(st),
		StoppedBy:   string(st.StoppedBy),
	}
}

func taskItem(info agent.BackgroundTaskInfo) activityTask {
	return activityTask{
		Description: info.Description,
		EndedAt:     rfc3339UTCOrNull(info.EndedAt),
		ID:          info.ID,
		StartedAt:   rfc3339UTC(info.StartedAt),
		Status:      string(info.Status),
		StoppedBy:   string(info.StoppedBy),
		Subagent:    info.Subagent,
	}
}

func cronItem(ci agent.CronInfo) activityCron {
	return activityCron{
		CreatedAt:   rfc3339UTC(ci.CreatedAt),
		ID:          ci.ID,
		LastFiredAt: rfc3339UTCOrNull(ci.LastFiredAt),
		Name:        ci.Name,
		NextFireAt:  rfc3339UTCOrNull(ci.NextFireAt),
		Prompt:      ci.Prompt,
		Spec:        ci.Spec,
	}
}

// activityWakeInfo computes the wake block under wakeMu (PM §4.3): enabled
// is the live auto-wake setting, capped means the chain budget is exhausted
// with items still queued, stalled means the last wake start failed and a
// user run must clear it, pending is the queued tagged-notification count.
func (s *Server) activityWakeInfo(sid string, pending int) activityWakeInfo {
	on, limit := s.wakePolicy()
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return activityWakeInfo{
		Capped:               limit > 0 && s.wakeChain[sid] >= limit && pending > 0,
		Enabled:              on,
		PendingNotifications: pending,
		Stalled:              s.wakeStalled[sid],
	}
}

// handleActivitySnapshot serves GET /api/sessions/{id}/activity. An unknown
// session yields the empty snapshot (200), matching GET /api/sessions/{id}.
func (s *Server) handleActivitySnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	sid := r.PathValue("id")
	act := s.agent.ActivityFor(sid)
	// Todos: live wins, else sidecar (PM §4.5, the goalForSession pattern).
	// A process that has not written todos for the session (TodosKnown=false,
	// the post-restart state) displays the persisted list with the sidecar's
	// updated_at; no sidecar keeps the never-written shape (empty items,
	// null updated_at).
	todos, todosUpdatedAt := act.Todos, act.TodosUpdatedAt
	if !act.TodosKnown {
		if items, at, ok := s.sessions.loadTodos(sid); ok {
			todos, todosUpdatedAt = items, at
		}
	}
	snap := activitySnapshot{
		Crons:      make([]activityCron, 0, len(act.Crons)),
		ServerTime: rfc3339UTC(time.Now()),
		SessionID:  sid,
		Shells:     make([]activityShell, 0, len(act.Shells)),
		Tasks:      make([]activityTask, 0, len(act.Tasks)),
		Todos:      activityTodos{Items: todos, UpdatedAt: rfc3339UTCOrNull(todosUpdatedAt)},
		Wake:       s.activityWakeInfo(sid, act.PendingNotifications),
	}
	for _, st := range act.Shells {
		snap.Shells = append(snap.Shells, shellItem(st))
	}
	for _, info := range act.Tasks {
		snap.Tasks = append(snap.Tasks, taskItem(info))
	}
	for _, ci := range act.Crons {
		snap.Crons = append(snap.Crons, cronItem(ci))
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleShellOutput serves GET /api/sessions/{id}/background/shells/{shell}/output.
func (s *Server) handleShellOutput(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	maxBytes, ok := tailBytesParam(r)
	if !ok {
		activityBadTailBytes(w)
		return
	}
	tail, truncated, total, st, err := s.agent.ShellTailFor(r.PathValue("id"), r.PathValue("shell"), maxBytes)
	if errors.Is(err, agent.ErrActivityNotFound) {
		activityNotFound(w)
		return
	}
	if err != nil {
		activityInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, activityOutput{
		ID:         st.ID,
		Output:     tail,
		Status:     shellStatusText(st),
		TotalBytes: total,
		Truncated:  truncated,
	})
}

// handleShellKill serves POST /api/sessions/{id}/background/shells/{shell}/kill:
// the panel's user stop. The seam attributes the stop (StopByUser) and
// quiet-queues the completion (D1), so a kill never wakes the session; an
// already-finished shell answers 200 with already_finished (the kill race
// resolves to the final state, never an error).
func (s *Server) handleShellKill(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	sid := r.PathValue("id")
	st, already, err := s.agent.KillShellFor(sid, r.PathValue("shell"), tools.StopByUser)
	if errors.Is(err, agent.ErrActivityNotFound) {
		activityNotFound(w)
		return
	}
	if err != nil {
		activityInternal(w)
		return
	}
	s.auditEvent(r, "bg_kill", sid)
	writeJSON(w, http.StatusOK, activityShellAction{AlreadyFinished: already, Shell: shellItem(st)})
}

// handleTaskOutput serves GET /api/sessions/{id}/background/tasks/{task}/output:
// the streamed lines while running, the final report after, "" when stopped.
func (s *Server) handleTaskOutput(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	maxBytes, ok := tailBytesParam(r)
	if !ok {
		activityBadTailBytes(w)
		return
	}
	tail, truncated, total, info, err := s.agent.TaskTailFor(r.PathValue("id"), r.PathValue("task"), maxBytes)
	if errors.Is(err, agent.ErrActivityNotFound) {
		activityNotFound(w)
		return
	}
	if err != nil {
		activityInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, activityOutput{
		ID:         info.ID,
		Output:     tail,
		Status:     string(info.Status),
		TotalBytes: total,
		Truncated:  truncated,
	})
}

// handleTaskStop serves POST /api/sessions/{id}/background/tasks/{task}/stop.
func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	sid := r.PathValue("id")
	info, already, err := s.agent.StopTaskFor(sid, r.PathValue("task"), tools.StopByUser)
	if errors.Is(err, agent.ErrActivityNotFound) {
		activityNotFound(w)
		return
	}
	if err != nil {
		activityInternal(w)
		return
	}
	s.auditEvent(r, "bg_stop", sid)
	writeJSON(w, http.StatusOK, activityTaskAction{AlreadyFinished: already, Task: taskItem(info)})
}

// handleCronDelete serves DELETE /api/sessions/{id}/background/crons/{cron}.
func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireActivityAPI(w) {
		return
	}
	sid := r.PathValue("id")
	id := r.PathValue("cron")
	if err := s.agent.DeleteCronFor(sid, id); err != nil {
		if errors.Is(err, agent.ErrActivityNotFound) {
			activityNotFound(w)
			return
		}
		activityInternal(w)
		return
	}
	s.auditEvent(r, "cron_delete", sid)
	writeJSON(w, http.StatusOK, activityCronDeleted{Deleted: true, ID: id})
}

// onActivityChanged is the agent seam's change observer (installed in New
// when the activity API is assembled) and the wake driver's announcement
// path: it publishes the signal-only activity_changed frame on the global
// stream (the same topic and Publish path session_state uses), carrying
// just the kind and the session id — clients re-fetch the authorized
// snapshot (D4). kind=todos additionally feeds the todo sidecar writer
// (B9, PM §4.5): the write is synchronous with the change, the same
// contract as the goal sidecar's subscriber. The observer contract is
// synchronous; the sidecar write and the local bus Publish never block, and
// a publish failure is a debug log, not a panic.
func (s *Server) onActivityChanged(session string, kind agent.ActivityKind) {
	if !s.activityAPI {
		return
	}
	if kind == agent.ActivityTodos && session != "" {
		// The unscoped "" bucket belongs to the local single-session
		// surfaces (TUI/REPL), not to a server session — no sidecar for it.
		s.persistTodos(session)
	}
	if _, err := s.hub.Publish(context.Background(), sessionStateTopic, Event{
		Type: EventActivityChanged,
		Data: activityChangedPayload{Kind: string(kind), SessionID: session},
	}); err != nil {
		s.log.Debug().Err(err).Str("session", session).Str("kind", string(kind)).
			Msg("activity_changed publish failed")
	}
}

// persistTodos writes the session's todo sidecar from the live store (the
// observer path — kind=todos). ok=false (never written this process) writes
// nothing; a cleared-to-empty list is a list and persists as empty.
func (s *Server) persistTodos(session string) {
	items, updatedAt, ok := s.agent.TodosFor(session)
	if !ok {
		return
	}
	s.sessions.saveTodos(session, items, updatedAt)
}

// restoreTodos seeds the live todo store from the sidecar when this process
// has no list for the session yet (the first run after a restart): live
// wins, else sidecar — a list cleared to empty is still a live list. Called
// BEFORE run.start so TodoWrite's merge baseline includes the pre-restart
// items. The restore itself is a todo change, so it re-fires kind=todos
// (idempotent sidecar rewrite, one activity_changed signal).
func (s *Server) restoreTodos(sid string) {
	if _, _, ok := s.agent.TodosFor(sid); ok {
		return
	}
	if items, _, ok := s.sessions.loadTodos(sid); ok {
		s.agent.SetTodosFor(sid, items)
	}
}
