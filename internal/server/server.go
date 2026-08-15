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
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/slashcmds"
	"github.com/projanvil/woca/internal/store"
)

// Config configures the web server.
type Config struct {
	// Agent is the coding agent that runs each conversation turn.
	Agent *agent.Agent
	// Static is the frontend filesystem (embedded or os.DirFS for development).
	Static fs.FS
	// Logger receives server diagnostics. May be zerolog.Nop().
	Logger zerolog.Logger
	// SessionDir persists session transcripts under this directory (empty
	// disables persistence, keeping sessions in memory only).
	SessionDir string
	// WorkDir is the agent's working directory, exposed via GET /api/workspace.
	WorkDir string
	// Settings is the persisted settings store backing /api/settings. Nil
	// disables the settings API.
	Settings *settings.Store
	// Memory is the memoryStore (currently informational; used by future
	// memory APIs).
	Memory *store.Store
	// MemoryEngine is the construction-time fallback engine for /api/memory:
	// handlers resolve the engine live per request via memEngineLive (the
	// agent's accessor first, which survives live rebuilds), so this field is
	// only consulted when Agent is nil (tests, embedders). Nil makes the
	// memory API degrade gracefully to engine "none".
	MemoryEngine memory.Engine
	// RebuildMemory rebuilds the memory engine after memory settings changes
	// submitted through /api/settings (optional).
	RebuildMemory func() error
	// RebuildModel rebuilds the agent's chat model after credential changes
	// submitted through /api/settings (optional).
	RebuildModel func() error
}

// Server serves the woca web UI and its HTTP API.
type Server struct {
	agent  *agent.Agent
	static fs.FS
	log    zerolog.Logger

	mu            sync.Mutex
	runs          map[string]*Run
	sessions      *sessionStore
	workdir       string
	settings      *settings.Store
	memory        *store.Store
	memEngine     memory.Engine
	rebuildMemory func() error
	rebuildModel  func() error

	// goalMu guards goalOwner: the session whose sidecar mirrors the live
	// goal (so a clear can delete the right file).
	goalMu    sync.Mutex
	goalOwner string
}

var (
	runSeq     int64
	sessionSeq int64
)

// New creates a Server.
func New(cfg Config) *Server {
	defaultWorkspace := ""
	if cfg.WorkDir != "" {
		if abs, err := filepath.Abs(cfg.WorkDir); err == nil {
			defaultWorkspace = abs
		} else {
			defaultWorkspace = cfg.WorkDir
		}
	}
	store := newSessionStore(cfg.SessionDir)
	store.defaultWorkspace = defaultWorkspace
	s := &Server{
		agent:         cfg.Agent,
		static:        cfg.Static,
		log:           cfg.Logger,
		runs:          map[string]*Run{},
		sessions:      store,
		workdir:       cfg.WorkDir,
		settings:      cfg.Settings,
		memory:        cfg.Memory,
		memEngine:     cfg.MemoryEngine,
		rebuildMemory: cfg.RebuildMemory,
		rebuildModel:  cfg.RebuildModel,
	}
	if cfg.Agent != nil {
		// Goal persistence: every live goal change mirrors to the owning
		// session's sidecar; a clear deletes it.
		cfg.Agent.SubscribeGoal(func(g *agent.Goal) {
			s.goalMu.Lock()
			owner := s.goalOwner
			if g != nil && g.SessionID != "" {
				owner = g.SessionID
				s.goalOwner = owner
			} else if g == nil {
				s.goalOwner = ""
			}
			s.goalMu.Unlock()
			if owner == "" {
				return
			}
			s.sessions.saveGoal(owner, g)
		})
	}
	return s
}

// Handler returns the full HTTP handler (API + embedded frontend).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/runs", s.handleCreateRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /api/runs/{id}/answer", s.handleAnswer)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/runs/{id}/input", s.handleQueueInput)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/goal", s.handleGetGoal)
	mux.HandleFunc("POST /api/sessions/{id}/goal", s.handleGoalCommand)
	mux.HandleFunc("GET /api/workspace", s.handleWorkspace)
	mux.HandleFunc("GET /api/workspace/file", s.handleWorkspaceFile)
	mux.HandleFunc("GET /api/completions", s.handleCompletions)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
	mux.HandleFunc("GET /api/memory", s.handleListMemory)
	mux.HandleFunc("GET /api/memory/stats", s.handleMemoryStats)
	mux.HandleFunc("POST /api/memory", s.handleRememberMemory)
	mux.HandleFunc("DELETE /api/memory/{id}", s.handleForgetMemory)
	mux.Handle("/", s.staticHandler())
	return mux
}

// Serve listens on addr and serves until ctx is cancelled, shutting down
// gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runAttachment is one image attached to a prompt (base64-encoded).
type runAttachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// attachmentLimits bound prompt attachments.
const (
	maxAttachments     = 6
	maxAttachmentBytes = 8 << 20 // decoded, across all attachments
)

var attachmentMIMEs = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true,
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string          `json:"session_id"`
		Prompt      string          `json:"prompt"`
		Workspace   string          `json:"workspace"`
		Attachments []runAttachment `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	human, err := humanMessage(req.Prompt, req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run, sessionID := s.startRun(req.SessionID, human, req.Workspace)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID(), "session_id": sessionID})
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

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional; empty workspace is fine
	resp := map[string]any{"session_id": newSessionID()}
	if req.Workspace != "" {
		resp["workspace"] = req.Workspace
		resp["workspace_name"] = workspaceDisplayName(req.Workspace)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	list, err := s.sessions.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []sessionSummary{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs := s.sessions.load(id)
	if msgs == nil {
		msgs = []messages.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "messages": msgs})
}

func (s *Server) handleWorkspace(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildWorkspaceTree(s.workdir))
}

// handleWorkspaceFile returns a single workspace file's content for the
// right-sidebar preview (bounded, text files only).
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	node, err := readWorkspaceFile(s.workdir, r.URL.Query().Get("path"))
	if err != nil {
		var status int
		switch err {
		case errNotPreviewable:
			status = http.StatusUnsupportedMediaType
		case errFileTooLarge:
			status = http.StatusRequestEntityTooLarge
		default:
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleCompletions lists slash commands (builtin + skills, for the chat
// input's "/" popup) and subagents (for the "@" popup).
func (s *Server) handleCompletions(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	resp := struct {
		Skills []entry `json:"skills"`
		Agents []entry `json:"agents"`
	}{Skills: []entry{}, Agents: []entry{}}
	// Builtin commands come first so a same-named skill cannot shadow them.
	for _, c := range slashcmds.Builtin() {
		resp.Skills = append(resp.Skills, entry{Name: c.Name, Description: c.Description})
	}
	if s.agent != nil {
		builtin := make(map[string]bool, len(resp.Skills))
		for _, e := range resp.Skills {
			builtin[e.Name] = true
		}
		for _, sk := range s.agent.Skills() {
			if builtin[sk.Name] {
				continue
			}
			resp.Skills = append(resp.Skills, entry{Name: sk.Name, Description: sk.Description})
		}
		for _, d := range s.agent.AgentDefinitions() {
			resp.Agents = append(resp.Agents, entry{Name: d.Name, Description: d.Description})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := run.subscribe()
	defer run.unsubscribe(sub)

	ctx := r.Context()
	for {
		select {
		case ev := <-sub.ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			flusher.Flush()
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		case <-ctx.Done():
			return
		}
	}
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
func (s *Server) startRun(sessionID string, human messages.Message, workspace string) (*Run, string) {
	id := fmt.Sprintf("run-%d", atomic.AddInt64(&runSeq, 1))
	if workspace == "" {
		workspace = s.workdir
	}
	// Run-scoped memory workspace: a values-only ctx decoration, so prefetch,
	// memory tools, and auto-retain all resolve the session workspace while
	// concurrent runs share the agent safely (ctx value > agent field >
	// workdir; see agent.WithMemoryWorkspace).
	ctx, cancel := context.WithCancel(agent.WithMemoryWorkspace(context.Background(), workspace))
	run := newRun(id, s.agent, ctx, cancel)

	s.mu.Lock()
	s.runs[id] = run
	s.mu.Unlock()

	sid := sessionID
	if sid == "" {
		sid = newSessionID()
	}
	history := s.sessions.load(sid)

	s.log.Info().Str("run", id).Str("session", sid).Msg("run started")
	s.refreshDebugLog()
	if obs := s.agent.Obs(); obs != nil {
		s.agent.SetObsRun(workspace, sid, id)
		obs.Log("info", "run_start", map[string]any{
			"prompt": previewForLog(human.Content),
		})
	}
	run.start(append(history, human), func(final []messages.Message) {
		_ = s.sessions.save(sid, stripSystem(final), workspace)
		s.logObsRunEnd(sid, id, final)
	})
	return run, sid
}

// logObsRunEnd records the run's completion and persists the message-level
// dump (full transcript + state summary) for observability.
func (s *Server) logObsRunEnd(sid, runID string, final []messages.Message) {
	obs := s.agent.Obs()
	if obs == nil {
		return
	}
	obs.Log("info", "run_end", map[string]any{"messages": len(final)})
	obs.DumpMessages(final, map[string]any{
		"sessions_dir": s.sessions.dir,
		"workdir":      s.workdir,
	})
}

// previewForLog bounds a prompt preview in observability events.
func previewForLog(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// newSessionID returns a unique, restart-safe session identifier.
func newSessionID() string {
	return fmt.Sprintf("sess-%x-%d", time.Now().UnixNano(), atomic.AddInt64(&sessionSeq, 1))
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

func (s *Server) lookupRun(id string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

// staticHandler serves the embedded frontend with an SPA fallback to index.html.
func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" || s.fileExists(r.URL.Path) {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: unknown paths render the single-page app.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

func (s *Server) fileExists(path string) bool {
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return true
	}
	f, err := s.static.Open(name)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
