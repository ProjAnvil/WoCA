// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/settings"
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
	// RebuildModel rebuilds the agent's chat model after credential changes
	// submitted through /api/settings (optional).
	RebuildModel func() error
}

// Server serves the woca web UI and its HTTP API.
type Server struct {
	agent  *agent.Agent
	static fs.FS
	log    zerolog.Logger

	mu           sync.Mutex
	runs         map[string]*Run
	sessions     *sessionStore
	workdir      string
	settings     *settings.Store
	memory       *store.Store
	rebuildModel func() error
}

var (
	runSeq     int64
	sessionSeq int64
)

// New creates a Server.
func New(cfg Config) *Server {
	return &Server{
		agent:        cfg.Agent,
		static:       cfg.Static,
		log:          cfg.Logger,
		runs:         map[string]*Run{},
		sessions:     newSessionStore(cfg.SessionDir),
		workdir:      cfg.WorkDir,
		settings:     cfg.Settings,
		memory:       cfg.Memory,
		rebuildModel: cfg.RebuildModel,
	}
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
	mux.HandleFunc("GET /api/workspace", s.handleWorkspace)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
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

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	run, sessionID := s.startRun(req.SessionID, req.Prompt)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID(), "session_id": sessionID})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"session_id": newSessionID()})
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

// startRun creates a run that continues the given session (or a new one when
// sessionID is empty) and persists the resulting transcript back to the
// session on completion.
func (s *Server) startRun(sessionID, prompt string) (*Run, string) {
	id := fmt.Sprintf("run-%d", atomic.AddInt64(&runSeq, 1))
	ctx, cancel := context.WithCancel(context.Background())
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
	run.start(append(history, messages.Human(prompt)), func(final []messages.Message) {
		_ = s.sessions.save(sid, stripSystem(final))
	})
	return run, sid
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
