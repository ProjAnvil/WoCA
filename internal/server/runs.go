// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/wocadb"
)

var attachmentMIMEs = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true,
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	// Dynamic off-host gate (design §10.2 M-3): an off-host deployment
	// without a live enabled sandbox backend fails closed — run creation
	// would land un-sandboxed tool execution somewhere.
	if s.runsOffHost && !s.sandboxEnabled() {
		http.Error(w, "sandbox execution is not available (no enabled sandbox backend)", http.StatusForbidden)
		return
	}
	var req struct {
		SessionID   string          `json:"session_id"`
		Prompt      string          `json:"prompt"`
		Workspace   string          `json:"workspace"`
		Attachments []runAttachment `json:"attachments"`
		Comments    []runComment    `json:"comments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	human, err := humanMessage(buildPrompt(req.Prompt, req.Comments), req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// File-level undo/redo commands execute against the harness directly
	// (no model turn), exactly like the TUI's /undo and /redo. The command
	// resolves the session's workspace so undo touches only that session's
	// edits (see tools.FileHistory).
	if len(req.Attachments) == 0 {
		switch strings.TrimSpace(req.Prompt) {
		case "/undo", "/redo":
			ws := req.Workspace
			if ws == "" {
				ws = s.workdir
			}
			ctx := agent.WithRunWorkspace(r.Context(), ws)
			var output string
			var err error
			if req.Prompt == "/redo" {
				output, err = s.agent.RedoFiles(ctx)
			} else {
				output, err = s.agent.UndoFiles(ctx)
			}
			if err != nil {
				output = "Error: " + err.Error()
			}
			undoN, redoN := s.agent.FileHistoryCounts(ctx)
			run, sessionID := s.startCommandRun(req.SessionID, human, req.Workspace,
				fmt.Sprintf("%s (%d undoable, %d redoable steps left)", output, undoN, redoN))
			writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID(), "session_id": sessionID})
			return
		}
	}

	s.auditEvent(r, "run_create", req.SessionID)
	run, sid, pos, err := s.startRun(r.Context(), req.SessionID, human, req.Workspace, req.Attachments)
	if err == errQueueFull {
		http.Error(w, "session run queue is full", http.StatusConflict)
		return
	}
	if errors.Is(err, errNoSandboxExecutor) || errors.Is(err, errSandboxBinding) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	state := "running"
	if pos > 0 {
		state = "queued"
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID(), "session_id": sid, "state": state, "queue_position": pos})
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := run.answer(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	run.cancelRun()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) lookupRun(id string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func shortHost() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host
}

func (s *Server) trackRunLease(run *Run, sid string) {
	// Mirror every emitted event into run_events so another instance (or a
	// reconnect after this process dies) can replay the stream. The SSE id
	// and the ledger seq are the same number.
	run.mirror = func(ev Event) {
		payload, err := json.Marshal(ev.Data)
		if err != nil {
			payload = []byte(`{}`)
		}
		mctx := context.WithoutCancel(run.ctx)
		if err := s.ledger.PutEvent(mctx, run.id, ev.Seq, ev.Type, string(payload)); err != nil {
			s.log.Warn().Str("run", run.id).Int64("seq", ev.Seq).Err(err).Msg("run event mirror failed")
		}
	}
	interval := runLeaseDuration / 3
	stop := make(chan struct{})
	// Chain the ledger finish onto the terminal event exactly once.
	prev := run.onFinish
	finishOnce := sync.OnceFunc(func() {
		close(stop)
		if s.ledger != nil {
			_ = s.ledger.Finish(context.WithoutCancel(run.ctx), run.id, s.workerID, run.finishType())
		}
	})
	run.onFinish = func() {
		finishOnce()
		if prev != nil {
			prev()
		}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-run.ctx.Done():
				return
			case <-ticker.C:
				if err := s.ledger.Heartbeat(context.WithoutCancel(run.ctx), run.id, s.workerID, runLeaseDuration); err != nil {
					// The lease was reaped (worker death from the ledger's
					// point of view) or handed to another worker: stop this
					// execution so the run is not double-run.
					s.log.Warn().Str("run", run.id).Str("session", sid).Err(err).Msg("run lease lost; cancelling local execution")
					run.cancel()
					return
				}
			}
		}
	}()
}

// handleStats reports runtime concurrency counters for threshold tuning.
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	notDone := 0
	var dropped int64
	for _, run := range s.runs {
		if !run.isDone() {
			notDone++
		}
		dropped += run.droppedCount()
	}
	s.mu.Unlock()
	queued, depths := s.admission.stats()
	// queued runs are registered but not finished; exclude them from active
	active := max(notDone-queued, 0)
	lspActive, lspBroken := 0, 0
	teammates, background := 0, 0
	if s.agent != nil {
		lspActive, lspBroken = s.agent.LSPStats()
		teammates = s.agent.ActiveTeammateCount()
		background = s.agent.RunningTaskCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active_runs":          active,
		"queued_runs":          queued,
		"session_queue_depths": depths,
		"teammates":            teammates,
		"background_tasks":     background,
		"sse_dropped":          dropped,
		"lsp":                  map[string]any{"active": lspActive, "broken": lspBroken},
	})
}

// RunStateCounts reports the role's run-state for the service gauges:
// active = registered runs not yet terminal, queued = runs waiting on
// their session admission gate.
func (s *Server) RunStateCounts() (active, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		if !run.isDone() {
			active++
		}
	}
	q, _ := s.admission.stats()
	return active, q
}

// runComment is one parked inline review comment (the Changes/preview
// surfaces collect them; they ride the next run's prompt as file:line
// context for the model).
type runComment struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// buildPrompt appends the parked review comments to the prompt as a
// structured block the model can anchor to files and lines.
func buildPrompt(prompt string, comments []runComment) string {
	if len(comments) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n<inline-review-comments>\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "- %s:%d: %s\n", c.File, c.Line, strings.TrimSpace(c.Text))
	}
	b.WriteString("</inline-review-comments>")
	return b.String()
}

// runAttachment is one image attached to a prompt (base64-encoded).
type runAttachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// humanMessage builds the prompt message, upgrading it to multimodal content
// blocks when image attachments are present.
func humanMessage(prompt string, atts []runAttachment) (messages.Message, error) {
	if len(atts) == 0 {
		return messages.Human(prompt), nil
	}
	if len(atts) > maxAttachments {
		return messages.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
	}
	blocks := []messages.ContentBlock{messages.TextBlock{Text: prompt}}
	total := 0
	for _, a := range atts {
		if !attachmentMIMEs[a.MimeType] {
			return messages.Message{}, fmt.Errorf("unsupported attachment type %q", a.MimeType)
		}
		decoded, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return messages.Message{}, fmt.Errorf("attachment %q is not valid base64", a.Name)
		}
		total += len(decoded)
		if total > maxAttachmentBytes {
			return messages.Message{}, fmt.Errorf("attachments exceed the %d MB limit", maxAttachmentBytes>>20)
		}
		blocks = append(blocks, messages.ImageBlock{MimeType: a.MimeType, Base64: a.Data})
	}
	return messages.Message{Role: messages.RoleHuman, Content: prompt, ContentBlocks: blocks}, nil
}

// handleBtwRun starts a side-question run ("/btw"): the same agent and
// session history answer the question, but the exchange is never persisted
// to the session transcript — the main conversation context stays clean.
// It streams over the regular run SSE endpoint and never touches the
// session run-state map, so the sidebar badge ignores it.
func (s *Server) handleBtwRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string `json:"prompt"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	s.auditEvent(r, "run_btw", r.PathValue("id"))
	human := messages.Human(req.Prompt)
	run, sid, _, history := s.newRegisteredRun(r.PathValue("id"), req.Workspace)
	run.emit(Event{Type: EventRunStarted, Data: map[string]any{}})
	run.start(append(history, human), func(final []messages.Message) {
		// Deliberately not persisted: /btw never enters the transcript.
		_ = final
	})
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID(), "session_id": sid})
}

// handleQueueInput buffers user input submitted while a run is in flight;
// the agent attaches it after the current tool round's results.
func (s *Server) handleQueueInput(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	s.agent.QueueInput(req.Text)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": s.agent.QueuedInputCount()})
}

// refreshDebugLog re-reads the persisted debug-log setting before each run so
// a toggle made from ANOTHER process (another serve instance, a TUI session)
// applies here dynamically; the store itself stays cheap while off. A store
// locked open by --debug-log ignores disables.
func (s *Server) refreshDebugLog() {
	if s.agent == nil || s.agent.Obs() == nil || s.settings == nil {
		return
	}
	s.agent.Obs().SetEnabled(s.settings.Load().DebugLog)
}

// startRun creates a run that continues the given session (or a new one when
// sessionID is empty) and persists the resulting transcript back to the
// session on completion. workspace records which project the session belongs
// to (empty falls back to the server's workdir) so the UI can group sessions.
// human is the fully-built prompt message (possibly with image content
// blocks).
// maxRetainedRuns caps how many finished runs stay in the registry: each Run
// keeps its full event history, so long-lived serve processes would otherwise
// grow without bound. Live subscribers hold the *Run directly, so pruning
// never cuts an active SSE stream.
const maxRetainedRuns = 64

// registerRun records a run for event subscription and prunes finished old
// runs beyond the retention cap. Runs that have not reached a terminal event
// are never dropped.
func (s *Server) registerRun(id string, run *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[id] = run
	s.runOrder = append(s.runOrder, id)
	for len(s.runOrder) > maxRetainedRuns {
		oldest := s.runs[s.runOrder[0]]
		if oldest == nil || !oldest.isDone() {
			break
		}
		delete(s.runs, s.runOrder[0])
		s.runOrder = s.runOrder[1:]
	}
}

// newRegisteredRun creates a Run, registers it on the server, and resolves
// the session id, workspace default and loaded history shared by the normal
// and command-run paths. The run context carries the run-scoped workspace
// (a values-only ctx decoration, so the system prompt environment, the
// file/shell tools, memory prefetch/tools and auto-retain all resolve the
// session workspace while concurrent runs share the agent safely; see
// agent.WithRunWorkspace and internal/runws).
func (s *Server) newRegisteredRun(sessionID, workspace string) (*Run, string, string, []messages.Message) {
	id := newRunID()
	if workspace == "" {
		workspace = s.workdir
	}
	ctx, cancel := context.WithCancel(agent.WithRunWorkspace(context.Background(), workspace))
	run := newRun(id, s.agent, ctx, cancel)
	// Telemetry correlation: every span inside this run (tools, LLM calls,
	// skill loads) carries the run/session identity.
	run.ctx = tracing.WithRun(run.ctx, id, sessionID)
	s.registerRun(id, run)
	sid := sessionID
	if sid == "" {
		sid = newSessionID()
	}
	history := s.sessions.load(run.ctx, sid)
	return run, sid, workspace, history
}

// errQueueFull is returned by startRun (and surfaced as HTTP 409) when a
// session already has maxQueuedPerSession runs waiting on its gate.
var errQueueFull = errors.New("session run queue is full")

// startRun creates, registers and launches a run behind the session
// admission gate. It returns the run, the session id and the queue
// position (0 = running immediately). errQueueFull is returned when the
// session already has maxQueuedPerSession runs waiting.
// In dispatch mode (DispatchToQueue) the run goes to the shared queue
// instead — see enqueueRun.
func (s *Server) startRun(ctx context.Context, sessionID string, human messages.Message, workspace string, atts []runAttachment) (*Run, string, int, error) {
	if s.dispatch {
		return s.enqueueRun(ctx, sessionID, human, workspace, atts)
	}
	run, sid, workspace, _ := s.newRegisteredRun(sessionID, workspace) // preloaded history discarded: reloaded after acquiring the gate so the previous run's persisted reply is visible
	s.log.Info().Str("run", run.id).Str("session", sid).Msg("run queued")
	s.refreshDebugLog()
	if obs := s.agent.Obs(); obs != nil {
		s.agent.SetObsRun(workspace, sid, run.id)
		obs.Log("info", "run_start", map[string]any{"session": sid, "run": run.id})
	}
	w, pos, ok := s.admission.enter(sid, func(p int) {
		run.emit(Event{Type: EventQueuePosition, Data: map[string]any{"position": p}})
	})
	if !ok {
		run.finish(EventError, map[string]any{"message": "session run queue is full"})
		return nil, sid, -1, errQueueFull
	}
	if pos > 0 {
		run.emit(Event{Type: EventRunQueued, Data: map[string]any{"position": pos}})
	}
	// Session run-state tracking: mark on start, flip on interrupt parking,
	// clear on the terminal event. clearSessionState is compare-and-clear, so
	// a queued run promoted behind this one never loses its row.
	run.onFinish = func() { s.clearSessionState(sid, run.id) }
	run.onPark = func() { s.markSessionState(sid, wocadb.SessionAwaiting, run.id) }
	run.onResume = func() { s.markSessionState(sid, wocadb.SessionRunning, run.id) }
	go func() {
		if w != nil && !s.admission.wait(run.ctx, sid, w) {
			// Cancelled while queued (cancelRun cancelled run.ctx) or shutdown.
			run.finish(EventDone, map[string]any{"cancelled": true})
			return
		}
		// Externalized run state (Phase 2): the database row is created when
		// the run actually starts executing — the one_active_run_per_session
		// constraint then holds across instances. A uniqueness failure means
		// another instance is running this session (crash-leftover rows are
		// reaped by their lease expiring), so this run errors out instead of
		// double-executing.
		if s.ledger != nil {
			thread := agent.NewThreadID()
			run.mu.Lock()
			run.thread = thread
			run.mu.Unlock()
			if err := s.ledger.Start(run.ctx, runstate.Run{
				RunID: run.id, SessionID: sid, ThreadID: thread,
			}, s.workerID, runLeaseDuration); err != nil {
				s.log.Warn().Str("run", run.id).Str("session", sid).Err(err).Msg("run rejected by the run ledger")
				run.finish(EventError, map[string]any{"message": "session is running on another instance"})
				s.admission.release(sid)
				return
			}
			s.trackRunLease(run, sid)
		}
		run.emit(Event{Type: EventRunStarted, Data: map[string]any{}})
		s.markSessionState(sid, wocadb.SessionRunning, run.id)
		history := s.sessions.load(run.ctx, sid)
		run.start(append(history, human), func(final []messages.Message) {
			if final != nil {
				// Completion persistence must survive run cancellation:
				// keep the ctx values, drop the cancellation.
				_ = s.sessions.save(context.WithoutCancel(run.ctx), sid, stripSystem(final), workspace)
				s.logObsRunEnd(sid, run.id, workspace, final)
			}
			// Correctness core: the session gate is released only after
			// onDone (including persistence) returns, so when the next
			// queued run is promoted the previous reply is guaranteed to
			// be persisted.
			s.admission.release(sid)
		})
	}()
	return run, sid, pos, nil
}

// errNoSandboxExecutor is returned by enqueueRun (and surfaced as HTTP 503)
// when no live sandbox-capable executor acked recently — a queued off-host
// run must never wait on an executor that cannot execute it sandboxed.
var errNoSandboxExecutor = errors.New("no sandbox-capable executor available")

// errSandboxBinding is returned by enqueueRun (and surfaced as HTTP 503)
// when a dispatched run's workspace could not be bound to a sandbox
// workspace identity (the executor proxy is unreachable or refused): the
// run fails closed instead of dispatching without a workspace the executor
// could route to.
var errSandboxBinding = errors.New("sandbox workspace binding unavailable")

// sandboxAckLiveWindow bounds how old an executor's capability ack may be
// to count as live (the executor heartbeat refreshes every 10s, design
// §10.2 M-4).
const sandboxAckLiveWindow = 15 * 1000 // ms

// hasLiveExecutor reports whether an executor acked the sandbox capability
// inside the live window (spec §3.5: only executor acks gate off-host
// runs — the worker role does not qualify).
func (s *Server) hasLiveExecutor() bool {
	if s.ackDB == nil {
		return false
	}
	acks, err := s.ackDB.ConfigAcks()
	if err != nil {
		s.log.Warn().Err(err).Msg("sandbox dispatch check: config_acks read failed")
		return false
	}
	now := wocadb.Now()
	for _, a := range acks {
		if a.Role != "executor" {
			continue
		}
		if now-a.UpdatedAt > sandboxAckLiveWindow {
			continue
		}
		if slices.Contains(strings.Split(a.Capabilities, ","), "sandbox") {
			return true
		}
	}
	return false
}

// enqueueRun dispatches a run to the shared queue for a woca executor to
// execute (Phase 3 wiring): the run is born queued with its prompt and
// execution environment in the config snapshot, the cross-instance
// one_active_run_per_session constraint is the admission gate, and SSE
// streams the ledger's run_events (serveLedgerRunEvents) no matter which
// instance executes. The returned Run is a stub holding only the id — it is
// deliberately not registered, so run-scoped SSE falls through to the
// ledger path (a registered run with no local execution would subscribe
// clients to a stream that never fires).
func (s *Server) enqueueRun(ctx context.Context, sessionID string, human messages.Message, workspace string, atts []runAttachment) (*Run, string, int, error) {
	// Dispatch precondition (design §4.5 / spec §3.5): an off-host run may
	// only queue when a live sandbox-capable executor exists — otherwise the
	// run would sit queued behind executors that cannot execute it sandboxed.
	if s.runsOffHost && !s.hasLiveExecutor() {
		return nil, sessionID, 0, errNoSandboxExecutor
	}
	sid := sessionID
	if sid == "" {
		sid = newSessionID()
	}
	if workspace == "" {
		workspace = s.workdir
	}
	// Workspace identity (Task 14): the run inherits the session's bound
	// sandbox workspace so the executor can route execution and the
	// per-workspace claim mutex holds. Sessions that predate the binding
	// plane backfill exactly once through the same allocate-or-bind API,
	// and the identity is written back to the session meta. A binding
	// failure fails the dispatch closed (503).
	wsID, err := s.resolveRunWorkspace(ctx, sid, workspace)
	if err != nil {
		return nil, sid, 0, err
	}
	// The queued run's context derives from the server lifecycle (rootCtx),
	// not the triggering request: a dispatched run must outlive its HTTP
	// request but still end at server shutdown.
	runCtx, runCancel := context.WithCancel(s.rootCtx)
	run := newRun(newRunID(), s.agent, runCtx, runCancel)
	thread := agent.NewThreadID()
	run.mu.Lock()
	run.thread = thread
	run.mu.Unlock()

	snap := runstate.RunSnapshot{
		Prompt:    human.Content,
		Workspace: workspace,
		SessionID: sid,
		ThreadID:  thread,
	}
	for _, a := range atts {
		snap.Attachments = append(snap.Attachments, runstate.Attachment{
			Name: a.Name, MimeType: a.MimeType, Data: a.Data,
		})
	}
	blob, err := runstate.EncodeSnapshot(snap)
	if err != nil {
		runCancel()
		return nil, sid, 0, err
	}
	s.log.Info().Str("run", run.id).Str("session", sid).Msg("run dispatched to queue")
	if err := s.ledger.Enqueue(runCtx, runstate.Run{
		RunID:          run.id,
		SessionID:      sid,
		ThreadID:       thread,
		ConfigSnapshot: blob,
		WorkspaceID:    wsID,
	}); err != nil {
		// The session constraint is the cross-instance admission gate; the
		// run-id collision is a process-wide outage signal.
		runCancel()
		if errors.Is(err, runstate.ErrActiveRun) {
			return nil, sid, -1, errQueueFull
		}
		return nil, sid, 0, err
	}
	// Wake the workers over the message plane (best-effort: the poll
	// fallback covers a missed notification). Nil plane = pure-PG posture.
	if s.msgplane != nil {
		s.msgplane.Notify(run.id)
	}
	// The session is busy from the client's point of view whether the run
	// executes here or on a worker; the worker's finish clears the row
	// (ClearIfRun is compare-and-clear, so it only lifts its own run).
	s.markSessionState(sid, wocadb.SessionRunning, run.id)
	// The stub is never registered, so no cancel endpoint or onFinish hook
	// releases runCtx: without a watcher each dispatched run's WithCancel
	// child stays registered on rootCtx until shutdown.
	go s.cancelQueuedRunAtTerminal(runCtx, runCancel, run.id)
	return run, sid, 0, nil
}

// resolveRunWorkspace returns the sandbox workspace identity a dispatched
// run executes in: the session's recorded binding (the normal path — a
// session created under the binding plane already carries it), or — for
// sessions persisted before the binding plane existed — one allocate-or-
// bind call backfilled into the session meta so later runs inherit. Empty
// means the run carries no workspace identity (the personal posture: no
// tenant plane, no executor volume). Binding failures are errSandboxBinding.
func (s *Server) resolveRunWorkspace(ctx context.Context, sessionID, fallback string) (string, error) {
	if wsID := s.sessions.workspaceIDOf(sessionID); wsID != "" {
		return wsID, nil
	}
	tenant, endpoint, ok := s.bindingTarget(ctx)
	if !ok {
		return "", nil
	}
	// The source workspace is the session's recorded one (the canonical
	// value createSession bound against), falling back to the request's
	// workspace only for sessions with no metadata at all.
	src := s.sessions.workspaceOf(sessionID)
	if src == "" {
		src = fallback
	}
	if src == "" {
		return "", nil
	}
	wsID, err := s.bindWorkspace(ctx, endpoint, tenant, workspaceSourceKey(tenant, src))
	if err != nil {
		return "", fmt.Errorf("%w: %w", errSandboxBinding, err)
	}
	s.sessions.setWorkspaceID(sessionID, wsID)
	return wsID, nil
}

// cancelQueuedRunAtTerminal releases a queue-dispatched run's context once
// its ledger row leaves the queued/running states. The row is the only
// terminal signal for an unregistered run; the watch polls at the SSE tail
// cadence and ends with runCtx itself (server shutdown or this cancel).
func (s *Server) cancelQueuedRunAtTerminal(ctx context.Context, cancel context.CancelFunc, runID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ledgerEventPoll):
		}
		row, ok, err := s.ledger.Row(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue // transient ledger failure: retry next tick
		}
		if !ok || (row.Status != runstate.StatusQueued && row.Status != runstate.StatusRunning) {
			cancel()
			return
		}
	}
}

// startCommandRun creates a run that completes immediately with a fixed
// output (harness-side commands like /undo and /redo): the reply streams as a
// normal delta and the transcript persists to the session like any turn.
func (s *Server) startCommandRun(sessionID string, human messages.Message, workspace, output string) (*Run, string) {
	run, sid, workspace, history := s.newRegisteredRun(sessionID, workspace)
	s.log.Info().Str("run", run.id).Str("session", sid).Str("command", human.Content).Msg("command run")
	go func() {
		defer run.cancel()
		run.emit(Event{Type: EventDelta, Data: map[string]any{"text": output + "\n"}})
		run.finish(EventDone, map[string]any{"cancelled": false})
		final := append(append([]messages.Message(nil), history...), human, messages.AI(output))
		_ = s.sessions.save(context.WithoutCancel(run.ctx), sid, stripSystem(final), workspace)
	}()
	return run, sid
}

// logObsRunEnd records the run's completion and persists the message-level
// dump (full transcript + state summary) for observability. The dump's
// workspace is the run's session workspace — not the server's startup
// directory — so debugging a multi-workspace run reads the directory the
// run actually operated in.
func (s *Server) logObsRunEnd(sid, runID, workspace string, final []messages.Message) {
	obs := s.agent.Obs()
	if obs == nil {
		return
	}
	obs.Log("info", "run_end", map[string]any{"messages": len(final)})
	obs.DumpMessages(final, map[string]any{
		"sessions_dir": s.sessions.dir,
		"workdir":      workspace,
	})
}

// newSessionID returns a unique, restart-safe session identifier.
func newSessionID() string {
	return fmt.Sprintf("sess-%x-%d", time.Now().UnixNano(), sessionSeq.Add(1))
}

// newRunID mints a run id unique across processes: the shared run ledger
// keys runs by run_id (Phase 2), and a bare per-process counter collides
// with rows left by previous instances — an enterprise restart used to
// fail its very first run on the stale primary key.
func newRunID() string {
	return fmt.Sprintf("run-%x-%d", time.Now().UnixNano(), runSeq.Add(1))
}

// stripSystem removes the leading system message from a transcript.
func stripSystem(msgs []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != messages.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

// newWorkerID mints this server instance's lease identity for the run
// ledger (owner of runs it executes; the reconciler re-queues runs whose
// lease expires).
func newWorkerID() string {
	return fmt.Sprintf("srv-%s-%d", shortHost(), os.Getpid())
}

// runLease is the heartbeat cadence for externalized runs: extend the lease
// every leaseInterval while the run lives; a lease lost to the reconciler
// (another instance adopted the run) cancels the local execution.
const runLeaseDuration = 90 * time.Second

// finishType returns the terminal event type ("done" or "error") recorded by
// finish; "" before the terminal event.
func (r *Run) finishType() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishKind
}

// ledgerEventPoll is the cross-instance tail interval.
const ledgerEventPoll = 150 * time.Millisecond

// serveLedgerRunEvents streams a foreign run's events from the run ledger:
// replay from the Last-Event-ID cursor, then poll for live events until the
// terminal one arrives. It reports false when the run is unknown (→ 404).
func (s *Server) serveLedgerRunEvents(w http.ResponseWriter, r *http.Request) bool {
	runID := r.PathValue("id")
	ctx := r.Context()
	row, ok, err := s.ledger.Row(ctx, runID)
	if err != nil {
		http.Error(w, "run ledger unavailable", http.StatusInternalServerError)
		return true
	}
	if !ok {
		return false
	}
	// Headers before the probe flush (same invariant as handleEvents: the
	// flush commits the response head, so the stream's content-type must
	// already be set).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher := http.NewResponseController(w)
	if err := flusher.Flush(); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return true
	}
	// Always emit an initial frame: the relay tunnel's streamRecorder
	// commits response headers at the first Write (design §6.3), so a
	// foreign run idling in the poll loop must still write — an SSE
	// comment is a no-op for EventSource but establishes the stream (and
	// arms the heartbeat).
	fmt.Fprint(w, ": connected\n\n")
	_ = flusher.Flush()

	sinceSeq, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	terminal := row.Status != runstate.StatusQueued && row.Status != runstate.StatusRunning
	for {
		events, err := s.ledger.Replay(ctx, runID, sinceSeq, 500)
		if err != nil {
			// The stream dies loudly; the client's resync path reloads.
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", EventResync, `{"reason":"ledger_unavailable"}`)
			_ = flusher.Flush() // best-effort: a dead client surfaces on the next write or ctx.Done
			return true
		}
		done := false
		for _, ev := range events {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.Payload)
			sinceSeq = ev.Seq
			if ev.Kind == EventDone || ev.Kind == EventError {
				done = true
			}
		}
		if len(events) > 0 {
			_ = flusher.Flush() // best-effort: a dead client surfaces on the next write or ctx.Done
		}
		if done {
			return true
		}
		if terminal {
			// The row ended without a mirrored terminal event (mirror gap):
			// tell the client to resync rather than hang.
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", EventResync, `{"reason":"terminal_without_event"}`)
			_ = flusher.Flush() // best-effort: a dead client surfaces on the next write or ctx.Done
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(ledgerEventPoll):
			// Re-check the row so a terminal transition ends the stream.
			row, ok, err = s.ledger.Row(ctx, runID)
			if err != nil || !ok {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", EventResync, `{"reason":"ledger_unavailable"}`)
				_ = flusher.Flush() // best-effort: a dead client surfaces on the next write or ctx.Done
				return true
			}
			terminal = row.Status != runstate.StatusQueued && row.Status != runstate.StatusRunning
		}
	}
}

// reconcileRunLeases re-queues runs whose owning instance lost its lease
// (process death). Called periodically from ServeStarted when the run
// ledger is configured; the Phase 3 worker claims what lands back in the
// queue.
func (s *Server) reconcileRunLeases(ctx context.Context) error {
	if s.ledger == nil {
		return nil
	}
	_, err := s.ledger.ReapExpired(ctx)
	return err
}
