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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/dpop"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/slashcmds"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/wocadb"
)

// Config configures the web server.
type Config struct {
	// Auth authenticates /api requests and resolves the tenancy context
	// (gateway.Authenticate). Nil trusts the local machine — the personal
	// edition behavior — through the same middleware code path server
	// editions use with stricter providers (design doc §3.3, §11.1).
	Auth authn.Provider
	// Authz authorizes /api requests against per-route scopes. Nil uses the
	// local scope-set authorizer.
	Authz authz.Authorizer
	// NativeAuth enables the native-client credential surface
	// (/api/native/*). Nil disables it (personal edition).
	NativeAuth *NativeAuth
	// RunLedger externalizes run state (runs + run_events) so multiple
	// instances sharing one database see each other's runs and adopt
	// expired leases (Phase 2). Nil keeps the in-process behavior only.
	RunLedger *runstate.Ledger
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
	// GrafanaURL is the ops console link surfaced through /api/health
	// (design §12.3: the business SPA embeds the IT pane as a link).
	// Empty hides it — the personal edition default.
	GrafanaURL string
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
	// CfgLive returns the agent's live effective configuration snapshot for
	// GET /api/agent (provider, model, thinking budget). Nil yields zero
	// values.
	CfgLive func() config.Config
	// SetThinkingBudget persists a new extended-thinking budget and rebuilds
	// the chat model (POST /api/agent/thinking). Nil disables the endpoint.
	SetThinkingBudget func(budget int) error
	// Quota fetches the provider quota summary (GET /api/quota). Nil reports
	// quota unavailable.
	Quota func(ctx context.Context) (quota.Info, error)
	// ApplyClaudeEnv persists a claude-skills / claude-mcp checkbox
	// selection, syncs the ~/.woca symlinks + mcp.json, and live-applies the
	// skill set (POST /api/claudeenv). Nil disables the write side of the
	// endpoint.
	ApplyClaudeEnv func(skills, mcp []string) error
	// Plugins snapshots the built-in plugin catalog with runtime install
	// state for GET /api/plugins (nil degrades to catalog-only output).
	Plugins func(enabled []string) []pluginrt.Status
	// InstallPlugin starts an async runtime install for one plugin
	// (POST /api/plugins/{name}/install; reinstall=true via the /reinstall
	// variant deletes the environment first — the upgrade path). Nil
	// disables the endpoints.
	InstallPlugin func(ctx context.Context, name string, reinstall bool) error
	// RunStateDB backs the durable per-session run-state records (woca.db
	// session_state table). Nil falls back to Settings' database; when
	// neither is present (database disabled, tests) an in-memory store
	// keeps the state API functional for this process only.
	RunStateDB *wocadb.DB
}

// Server serves the woca web UI and its HTTP API.
type Server struct {
	agent      *agent.Agent
	static     fs.FS
	log        zerolog.Logger
	auth       authn.Provider
	authz      authz.Authorizer
	nativeAuth *NativeAuth
	ledger     *runstate.Ledger
	workerID   string

	mu             sync.Mutex
	runs           map[string]*Run
	runOrder       []string // registration order, backs finished-run pruning
	sessions       *sessionStore
	admission      *admission
	state          sessionStateStore // durable per-session run state (running/awaiting)
	owner          string            // "pid@host" stamped on state rows
	hub            eventbus.Bus      // global SSE broadcast (/api/events)
	workdir        string
	grafanaURL     string // ops console link from WOCA_GRAFANA_URL (design §12.3)
	settings       *settings.Store
	memory         *store.Store
	memEngine      memory.Engine
	rebuildMemory  func() error
	rebuildModel   func() error
	cfgLive        func() config.Config
	setThinking    func(budget int) error
	quotaFetch     func(ctx context.Context) (quota.Info, error)
	applyClaudeEnv func(skills, mcp []string) error
	plugins        func(enabled []string) []pluginrt.Status
	installPlugin  func(ctx context.Context, name string, reinstall bool) error

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
	stateDB := cfg.RunStateDB
	if stateDB == nil && cfg.Settings != nil {
		stateDB = cfg.Settings.DB()
	}
	var state sessionStateStore = newMemSessionState()
	if stateDB != nil {
		state = &dbSessionState{db: stateDB}
	}
	auth := cfg.Auth
	if auth == nil {
		auth = authn.NewLocal()
	}
	authorizer := cfg.Authz
	if authorizer == nil {
		authorizer = authz.NewLocal()
	}
	s := &Server{
		agent:          cfg.Agent,
		static:         cfg.Static,
		log:            cfg.Logger,
		auth:           auth,
		authz:          authorizer,
		nativeAuth:     cfg.NativeAuth,
		ledger:         cfg.RunLedger,
		workerID:       newWorkerID(),
		runs:           map[string]*Run{},
		sessions:       store,
		admission:      newAdmission(),
		state:          state,
		owner:          newStateOwner(),
		hub:            eventbus.NewLocalWithRetention(1024),
		workdir:        cfg.WorkDir,
		grafanaURL:     cfg.GrafanaURL,
		settings:       cfg.Settings,
		memory:         cfg.Memory,
		memEngine:      cfg.MemoryEngine,
		rebuildMemory:  cfg.RebuildMemory,
		rebuildModel:   cfg.RebuildModel,
		cfgLive:        cfg.CfgLive,
		setThinking:    cfg.SetThinkingBudget,
		quotaFetch:     cfg.Quota,
		applyClaudeEnv: cfg.ApplyClaudeEnv,
		plugins:        cfg.Plugins,
		installPlugin:  cfg.InstallPlugin,
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
	// Rows whose process died between mark and clear (a crash mid-run) must
	// not pin sessions as running forever.
	s.reconcileStaleSessionState()
	return s
}

// Handler returns the full HTTP handler (API + embedded frontend).
//
// Every /api route passes the same chain: authentication (provider
// injectable, design doc §11.3 red line 1), the local-tenant gate on host
// surfaces (Phase 1 posture, see gateway.RequireLocalTenant), and a
// per-route scope check. The health probe is the only exempt /api route
// (infrastructure liveness).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	if s.nativeAuth != nil {
		s.registerNativeAuth(mux)
	}
	s.route(mux, "POST /api/runs", authz.ScopeRunsWrite, s.handleCreateRun)
	s.route(mux, "GET /api/runs/{id}/events", authz.ScopeRunsRead, s.handleEvents)
	s.route(mux, "POST /api/runs/{id}/answer", authz.ScopeRunsWrite, s.handleAnswer)
	s.route(mux, "POST /api/runs/{id}/cancel", authz.ScopeRunsWrite, s.handleCancel)
	s.route(mux, "POST /api/runs/{id}/input", authz.ScopeRunsWrite, s.handleQueueInput)
	s.route(mux, "POST /api/sessions", authz.ScopeSessionsWrite, s.handleCreateSession)
	s.route(mux, "GET /api/sessions", authz.ScopeSessionsRead, s.handleListSessions)
	s.route(mux, "GET /api/events", authz.ScopeRunsRead, s.handleGlobalEvents)
	s.route(mux, "GET /api/sessions/{id}", authz.ScopeSessionsRead, s.handleGetSession)
	s.route(mux, "DELETE /api/sessions/{id}", authz.ScopeSessionsWrite, s.handleDeleteSession)
	s.route(mux, "GET /api/sessions/{id}/goal", authz.ScopeSessionsRead, s.handleGetGoal)
	s.route(mux, "POST /api/sessions/{id}/goal", authz.ScopeSessionsWrite, s.handleGoalCommand)
	s.route(mux, "GET /api/sessions/{id}/context", authz.ScopeSessionsRead, s.handleSessionContext)
	s.route(mux, "GET /api/workspace", authz.ScopeWorkspaceRead, s.handleWorkspace)
	s.route(mux, "GET /api/workspace/file", authz.ScopeWorkspaceRead, s.handleWorkspaceFile)
	s.route(mux, "GET /api/completions", authz.ScopeSessionsRead, s.handleCompletions)
	s.route(mux, "GET /api/agent", authz.ScopeSettingsRead, s.handleGetAgent)
	s.route(mux, "POST /api/agent/permissions", authz.ScopeAgentWrite, s.handleSetPermissionMode)
	s.route(mux, "POST /api/agent/thinking", authz.ScopeAgentWrite, s.handleSetThinking)
	s.route(mux, "GET /api/quota", authz.ScopeSettingsRead, s.handleQuota)
	s.route(mux, "GET /api/claudeenv", authz.ScopeSettingsRead, s.handleGetClaudeEnv)
	s.route(mux, "PUT /api/claudeenv", authz.ScopeSettingsWrite, s.handlePutClaudeEnv)
	s.route(mux, "GET /api/plugins", authz.ScopePluginsRead, s.handleGetPlugins)
	s.route(mux, "PUT /api/plugins", authz.ScopePluginsWrite, s.handlePutPlugins)
	s.route(mux, "POST /api/plugins/{name}/install", authz.ScopePluginsWrite, s.handleInstallPlugin)
	s.route(mux, "POST /api/plugins/{name}/reinstall", authz.ScopePluginsWrite, s.handleInstallPlugin)
	s.route(mux, "GET /api/settings", authz.ScopeSettingsRead, s.handleGetSettings)
	s.route(mux, "PUT /api/settings", authz.ScopeSettingsWrite, s.handleUpdateSettings)
	s.route(mux, "GET /api/memory", authz.ScopeMemoryRead, s.handleListMemory)
	s.route(mux, "GET /api/memory/stats", authz.ScopeMemoryRead, s.handleMemoryStats)
	s.route(mux, "GET /api/stats", authz.ScopeSettingsRead, s.handleStats)
	s.route(mux, "POST /api/memory", authz.ScopeMemoryWrite, s.handleRememberMemory)
	s.route(mux, "DELETE /api/memory/{id}", authz.ScopeMemoryWrite, s.handleForgetMemory)
	mux.Handle("/", s.staticHandler())
	if s.nativeAuth != nil {
		return gateway.Authenticate(s.auth, tenant.NewSessionProvider(s.nativeAuth.tenantStore(), dpop.NewVerifier()))(
			gateway.RequireLocalTenant()(mux),
		)
	}
	return gateway.Authenticate(s.auth)(
		gateway.RequireLocalTenant()(mux),
	)
}

// route registers h at pattern with a required scope (gateway.RequireScope).
func (s *Server) route(mux *http.ServeMux, pattern, scope string, h http.HandlerFunc) {
	mux.Handle(pattern, gateway.RequireScope(s.authz, scope)(h))
}

// Serve listens on addr and serves until ctx is cancelled, shutting down
// gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	return s.ServeStarted(ctx, srv, errCh)
}

// ServeStarted drives an http.Server the caller already started (the caller
// may have bound the listener and served a startup handler long before the
// agent was built): it waits for serveErr or ctx cancellation, then stops
// admission and shuts the server down gracefully.
func (s *Server) ServeStarted(ctx context.Context, srv *http.Server, serveErr <-chan error) error {
	// Periodic lease reconciliation: re-queue runs whose owning instance
	// died, so another instance (or the Phase 3 worker) can take them over.
	if s.ledger != nil {
		go func() {
			ticker := time.NewTicker(runLeaseDuration / 2)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := s.reconcileRunLeases(context.WithoutCancel(ctx)); err != nil {
						s.log.Warn().Err(err).Msg("run lease reconcile failed")
					}
				}
			}
		}()
	}
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.admission.stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{"ok": true}
	if s.grafanaURL != "" {
		payload["grafana_url"] = s.grafanaURL
	}
	writeJSON(w, http.StatusOK, payload)
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
	active := notDone - queued // queued runs are registered but not finished; exclude them from active
	if active < 0 {
		active = 0
	}
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

	run, sid, pos, err := s.startRun(req.SessionID, human, req.Workspace)
	if err == errQueueFull {
		http.Error(w, "session run queue is full", http.StatusConflict)
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
	ws := expandHomePath(req.Workspace)
	if ws == "" {
		ws = s.workdir
	}
	id := newSessionID()
	// Persist immediately (empty transcript + index meta): the sidebar lists
	// sessions from the persisted index, so an unpersisted session — and with
	// it a freshly added workspace group — would vanish on page refresh.
	// Before, the session first materialized when its initial run completed.
	_ = s.sessions.save(id, nil, ws)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     id,
		"workspace":      ws,
		"workspace_name": workspaceDisplayName(ws),
	})
}

// expandHomePath resolves a leading ~ or ~/ so workspaces typed as shorthand
// in the UI persist (and group) by their absolute path.
func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
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
	// Decorate with the durable run state so a fresh page load (and other
	// processes sharing the database) can see busy sessions.
	states := s.state.Snapshot()
	for i := range list {
		if st, ok := states[list[i].ID]; ok {
			list[i].RunStatus = st.Status
			list[i].RunID = st.RunID
		}
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

// handleDeleteSession removes a session (transcript, goal sidecar, index
// entry). Deleting an unknown id still succeeds — the client's goal is for
// the session to be gone.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.sessions.delete(id)
	s.forgetSessionState(id)
	s.log.Info().Str("session", id).Msg("session deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// workspaceForRequest resolves the directory a workspace-scoped request
// targets: the session workspace the client passes as ?workspace=, else the
// server's startup directory. This is what keeps the file sidebar in sync
// with the selected session's project in multi-workspace serve mode.
func (s *Server) workspaceForRequest(r *http.Request) string {
	if ws := r.URL.Query().Get("workspace"); ws != "" {
		return ws
	}
	return s.workdir
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildWorkspaceTree(s.workspaceForRequest(r)))
}

// handleWorkspaceFile returns a single workspace file's content for the
// right-sidebar preview (bounded, text files only).
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	node, err := readWorkspaceFile(s.workspaceForRequest(r), r.URL.Query().Get("path"))
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
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
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
		// Skills and subagent definitions are workspace-scoped: the client
		// passes the session workspace so the autocomplete matches what the
		// model can actually invoke in that session.
		ctx := agent.WithRunWorkspace(r.Context(), r.URL.Query().Get("workspace"))
		builtin := make(map[string]bool, len(resp.Skills))
		for _, e := range resp.Skills {
			builtin[e.Name] = true
		}
		for _, sk := range s.agent.SkillsIn(ctx) {
			if builtin[sk.Name] {
				continue
			}
			resp.Skills = append(resp.Skills, entry{Name: sk.Name, Description: sk.Description})
		}
		for _, d := range s.agent.AgentDefinitionsIn(ctx) {
			resp.Agents = append(resp.Agents, entry{Name: d.Name, Description: d.Description})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		// The run may live on another instance sharing the database:
		// stream it from the run ledger instead (Phase 2 cross-instance
		// SSE).
		if s.ledger != nil && s.serveLedgerRunEvents(w, r) {
			return
		}
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

	// Last-Event-ID carries the last seq the client saw; resuming replays
	// only the missed tail (or signals a gap outside the retention window).
	lastSeq, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	var sub *subscriber
	var gap bool
	if lastSeq > 0 {
		sub, gap = run.subscribeSince(lastSeq)
	} else {
		sub = run.subscribe()
	}
	defer run.unsubscribe(sub)
	if gap {
		fmt.Fprintf(w, "event: resync\ndata: %s\n\n", `{"reason":"history_gap"}`)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case ev := <-sub.ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); err != nil {
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
	run.ctx = telemetry.WithRun(run.ctx, id, sessionID)
	s.registerRun(id, run)
	sid := sessionID
	if sid == "" {
		sid = newSessionID()
	}
	history := s.sessions.load(sid)
	return run, sid, workspace, history
}

// errQueueFull is returned by startRun (and surfaced as HTTP 409) when a
// session already has maxQueuedPerSession runs waiting on its gate.
var errQueueFull = errors.New("session run queue is full")

// startRun creates, registers and launches a run behind the session
// admission gate. It returns the run, the session id and the queue
// position (0 = running immediately). errQueueFull is returned when the
// session already has maxQueuedPerSession runs waiting.
func (s *Server) startRun(sessionID string, human messages.Message, workspace string) (*Run, string, int, error) {
	run, sid, workspace, _ := s.newRegisteredRun(sessionID, workspace) // preloaded history discarded: reloaded after acquiring the gate so the previous run's persisted reply is visible
	s.log.Info().Str("run", run.id).Str("session", sid).Msg("run queued")
	s.refreshDebugLog()
	if obs := s.agent.Obs(); obs != nil {
		s.agent.SetObsRun(workspace, sid, run.id)
		obs.Log("info", "run_start", map[string]any{"session": sid, "run": run.id})
	}
	w, pos, ok := s.admission.enter(sid, func(p int) {
		run.emit(Event{Type: "queue_position", Data: map[string]any{"position": p}})
	})
	if !ok {
		run.finish("error", map[string]any{"message": "session run queue is full"})
		return nil, sid, -1, errQueueFull
	}
	if pos > 0 {
		run.emit(Event{Type: "run_queued", Data: map[string]any{"position": pos}})
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
			run.finish("done", map[string]any{"cancelled": true})
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
				run.finish("error", map[string]any{"message": "session is running on another instance"})
				s.admission.release(sid)
				return
			}
			s.trackRunLease(run, sid)
		}
		run.emit(Event{Type: "run_started", Data: map[string]any{}})
		s.markSessionState(sid, wocadb.SessionRunning, run.id)
		history := s.sessions.load(sid)
		run.start(append(history, human), func(final []messages.Message) {
			if final != nil {
				_ = s.sessions.save(sid, stripSystem(final), workspace)
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

// startCommandRun creates a run that completes immediately with a fixed
// output (harness-side commands like /undo and /redo): the reply streams as a
// normal delta and the transcript persists to the session like any turn.
func (s *Server) startCommandRun(sessionID string, human messages.Message, workspace, output string) (*Run, string) {
	run, sid, workspace, history := s.newRegisteredRun(sessionID, workspace)
	s.log.Info().Str("run", run.id).Str("session", sid).Str("command", human.Content).Msg("command run")
	go func() {
		defer run.cancel()
		run.emit(Event{Type: "delta", Data: map[string]any{"text": output + "\n"}})
		run.finish("done", map[string]any{"cancelled": false})
		final := append(append([]messages.Message(nil), history...), human, messages.AI(output))
		_ = s.sessions.save(sid, stripSystem(final), workspace)
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

// newRunID mints a run id unique across processes: the shared run ledger
// keys runs by run_id (Phase 2), and a bare per-process counter collides
// with rows left by previous instances — an enterprise restart used to
// fail its very first run on the stale primary key.
func newRunID() string {
	return fmt.Sprintf("run-%x-%d", time.Now().UnixNano(), atomic.AddInt64(&runSeq, 1))
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
	// charset=utf-8 is explicit so any client (not just fetch) decodes the
	// Chinese descriptions/detail strings correctly.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// newWorkerID mints this server instance's lease identity for the run
// ledger (owner of runs it executes; the reconciler re-queues runs whose
// lease expires).
func newWorkerID() string {
	return fmt.Sprintf("srv-%s-%d", shortHost(), os.Getpid())
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

// runLease is the heartbeat cadence for externalized runs: extend the lease
// every leaseInterval while the run lives; a lease lost to the reconciler
// (another instance adopted the run) cancels the local execution.
const runLeaseDuration = 90 * time.Second

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
	var once sync.Once
	run.onFinish = func() {
		once.Do(func() {
			close(stop)
			if s.ledger != nil {
				_ = s.ledger.Finish(context.WithoutCancel(run.ctx), run.id, s.workerID, run.finishType())
			}
		})
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
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sinceSeq, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	terminal := row.Status != runstate.StatusQueued && row.Status != runstate.StatusRunning
	for {
		events, err := s.ledger.Replay(ctx, runID, sinceSeq, 500)
		if err != nil {
			// The stream dies loudly; the client's resync path reloads.
			fmt.Fprintf(w, "event: resync\ndata: %s\n\n", `{"reason":"ledger_unavailable"}`)
			flusher.Flush()
			return true
		}
		done := false
		for _, ev := range events {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.Payload)
			sinceSeq = ev.Seq
			if ev.Kind == "done" || ev.Kind == "error" {
				done = true
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if done {
			return true
		}
		if terminal {
			// The row ended without a mirrored terminal event (mirror gap):
			// tell the client to resync rather than hang.
			fmt.Fprintf(w, "event: resync\ndata: %s\n\n", `{"reason":"terminal_without_event"}`)
			flusher.Flush()
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(ledgerEventPoll):
			// Re-check the row so a terminal transition ends the stream.
			row, ok, err = s.ledger.Row(ctx, runID)
			if err != nil || !ok {
				fmt.Fprintf(w, "event: resync\ndata: %s\n\n", `{"reason":"ledger_unavailable"}`)
				flusher.Flush()
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
