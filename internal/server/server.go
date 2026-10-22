// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projanvil/woca/internal/platform/httpx"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/fsb"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/projanvil/woca/internal/wocadb"
)

// Config configures the web server.
type Config struct {
	// Auth authenticates /api requests and resolves the tenancy context
	// (gateway.Authenticate). Nil trusts the local machine — the personal
	// edition behavior — through the same middleware code path server
	// editions use with stricter providers (design doc §3.3, §11.1).
	Auth authn.Provider
	// ObservabilityConsoleManaged marks the enterprise posture (serve
	// --auth apikey and the bridge edition): observability plugins are
	// owned by the console, and user writes through this API are
	// rejected regardless of role.
	ObservabilityConsoleManaged bool
	// Authz authorizes /api requests against per-route scopes. Nil uses the
	// local scope-set authorizer.
	Authz authz.Authorizer
	// NativeAuth enables the native-client credential surface
	// (/api/native/*). Nil disables it (personal edition).
	NativeAuth NativeAuth
	// RunLedger externalizes run state (runs + run_events) so multiple
	// instances sharing one database see each other's runs and adopt
	// expired leases (Phase 2). Nil keeps the in-process behavior only.
	RunLedger *runstate.Ledger
	// DispatchToQueue routes run creation to the shared queue instead of
	// executing in-process (Phase 3 wiring): the gateway Enqueues the run
	// (prompt + environment ride the config snapshot) and a woca-worker
	// claims and executes it; SSE streams replay the ledger's run_events
	// regardless of which instance executes. Requires RunLedger; when either
	// is unset the server keeps the in-process posture.
	DispatchToQueue bool
	// RunsOffHost opens the /api/runs surface to non-local tenants: runs are
	// dispatched to workers whose tool execution is sandboxed, so run
	// creation and event streaming no longer touch the host. All other
	// host-surface routes (workspaces, settings, memory) stay local-gated.
	RunsOffHost bool
	// SandboxEnabled reports live whether a sandbox backend is enabled
	// (the enterprise entry injects a query against the materialized
	// sandbox_backends table). It refines RunsOffHost per request (design
	// §10.2 M-3): an off-host deployment without a working sandbox fails
	// closed — POST /api/runs returns 403 — and queued runs additionally
	// require a live sandbox-capable worker (503 otherwise). Nil counts
	// as no sandbox.
	SandboxEnabled func() bool
	// MsgPlane is the run dispatch message plane (internal/ee/msgplane). Nil
	// keeps pure-PG queuing; with a plane, dispatched runs additionally
	// notify NATS so idle workers wake instantly (progressive overlay —
	// the PG poll fallback stays the correctness net).
	MsgPlane MsgPlane
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
	// skill set (POST /api/claude-env). Nil disables the write side of the
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
	// FS is the workspace folder-browser service (product design §4.1):
	// directory-only listing, mkdir, watch, and validation jailed to the
	// configured roots. Nil disables the /api/fs/* routes.
	FS *fsb.Service
	// Extras mounts additional routes on the API mux before the gateway
	// middleware wraps it (edition-specific surfaces such as the bridge
	// login routes). Nil mounts nothing.
	Extras func(mux *http.ServeMux)
	// AnonymousAPIPaths are exact /api routes the deployment form mounts
	// as bootstrap exchanges (login, activation): exempt from
	// authentication beyond the health probe. The form that mounts the
	// route declares it — core learns no route names.
	AnonymousAPIPaths []string
	// HostGateExemptPrefixes are path prefixes the deployment form opens
	// past the local-tenant gate: its anonymous bootstrap routes and
	// session-lifecycle surfaces that carry no host data.
	HostGateExemptPrefixes []string
	// StreamReauth periodically revalidates a stream's credential after
	// connect (revocation closing): on error the stream emits an
	// auth_revoked event carrying the enumerated code and closes. Nil
	// keeps connect-time-only validation.
	StreamReauth func(credential string) error
	// Audit records a local audit event for sensitive operations (run
	// creation, workspace writes) with the acting identity; the bridge
	// wires it to its audit table (product design R5). Nil skips auditing.
	Audit func(actor, action, detail string)
	// ModelCatalog overrides the local preset list for deployments whose
	// authoritative model catalog lives behind another plane (nil = the
	// local database; the selection surface reads whatever this returns).
	ModelCatalog func(ctx context.Context) ([]wocadb.ModelPreset, error)
	// ResolveModel resolves a preferred model against the authoritative
	// catalog: (preset, its credential, ok). Deployments whose catalog is
	// remote inject a fetcher; nil resolves against the local database.
	// The credential travels only between the catalog authority and this
	// process — it never appears on a client surface.
	ResolveModel func(ctx context.Context, id, provider, model string) (wocadb.ModelPreset, string, bool)
}

// Server serves the woca web UI and its HTTP API.
type Server struct {
	agent      *agent.Agent
	static     fs.FS
	log        zerolog.Logger
	auth       authn.Provider
	obsManaged bool
	authz      authz.Authorizer
	nativeAuth NativeAuth
	fs         *fsb.Service
	extras     func(mux *http.ServeMux)
	audit      func(actor, action, detail string)
	// anonymousAPI / hostGateExempt carry the deployment form's declared
	// exemptions: bootstrap exchanges (login, activation) the form mounts
	// and prefixes its session surfaces need past the local-tenant gate.
	// Core never hardcodes an edition's route names.
	anonymousAPI   []string
	hostGateExempt []string
	streamReauth   func(string) error
	ledger         *runstate.Ledger
	workerID       string
	// dispatch enqueues runs for a worker to execute (Phase 3 wiring);
	// runsOffHost opens the run surface to remote tenants; sandboxEnabled
	// is its live refinement (see Config.SandboxEnabled).
	dispatch       bool
	runsOffHost    bool
	sandboxEnabled func() bool
	ackDB          *wocadb.DB // config_acks reads for worker-capability dispatch checks

	mu            sync.Mutex
	runs          map[string]*Run
	runOrder      []string // registration order, backs finished-run pruning
	msgplane      MsgPlane
	sessions      *sessionStore
	admission     *admission
	state         sessionStateStore // durable per-session run state (running/awaiting)
	owner         string            // "pid@host" stamped on state rows
	hub           eventbus.Bus      // global SSE broadcast (/api/events)
	workdir       string
	grafanaURL    string // ops console link from WOCA_GRAFANA_URL (design §12.3)
	settings      *settings.Store
	memory        *store.Store
	memEngine     memory.Engine
	rebuildMemory func() error
	rebuildModel  func() error
	// modelCatalog/resolveModel: the authoritative model catalog seam
	// (nil = the local database; see Config).
	modelCatalog   func(ctx context.Context) ([]wocadb.ModelPreset, error)
	resolveModel   func(ctx context.Context, id, provider, model string) (wocadb.ModelPreset, string, bool)
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

	// rootCtx is the server lifecycle context: contexts that must outlive
	// their triggering request but die with the server (e.g. dispatched
	// queue runs) derive from it. rootCancel fires in shutdown.
	rootCtx    context.Context
	rootCancel context.CancelFunc
}

var (
	runSeq     atomic.Int64
	sessionSeq atomic.Int64
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
	store.log = cfg.Logger
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
	rootCtx, rootCancel := context.WithCancel(context.Background())
	s := &Server{
		agent:          cfg.Agent,
		static:         cfg.Static,
		log:            cfg.Logger,
		auth:           auth,
		obsManaged:     cfg.ObservabilityConsoleManaged,
		authz:          authorizer,
		nativeAuth:     cfg.NativeAuth,
		anonymousAPI:   cfg.AnonymousAPIPaths,
		hostGateExempt: cfg.HostGateExemptPrefixes,
		streamReauth:   cfg.StreamReauth,
		fs:             cfg.FS,
		extras:         cfg.Extras,
		audit:          cfg.Audit,
		ledger:         cfg.RunLedger,
		msgplane:       cfg.MsgPlane,
		workerID:       newWorkerID(),
		dispatch:       cfg.DispatchToQueue && cfg.RunLedger != nil,
		runsOffHost:    cfg.RunsOffHost,
		sandboxEnabled: cfg.SandboxEnabled,
		ackDB:          stateDB,
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
		modelCatalog:   cfg.ModelCatalog,
		resolveModel:   cfg.ResolveModel,
		cfgLive:        cfg.CfgLive,
		setThinking:    cfg.SetThinkingBudget,
		quotaFetch:     cfg.Quota,
		applyClaudeEnv: cfg.ApplyClaudeEnv,
		plugins:        cfg.Plugins,
		installPlugin:  cfg.InstallPlugin,
		rootCtx:        rootCtx,
		rootCancel:     rootCancel,
	}
	// A nil SandboxEnabled counts as "no sandbox" (fail-closed), never as
	// a nil-func panic on the request path.
	if s.sandboxEnabled == nil {
		s.sandboxEnabled = func() bool { return false }
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
		s.nativeAuth.RegisterRoutes(mux)
	}
	if s.fs != nil {
		s.registerFS(mux)
	}
	if s.extras != nil {
		s.extras(mux)
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
	// Checkpoint/changes plane (web review tab): pending-change diffs,
	// workspace rollback, conversation truncation and /btw side runs.
	s.route(mux, "GET /api/workspace/changes", authz.ScopeWorkspaceRead, s.handleWorkspaceChanges)
	s.route(mux, "POST /api/workspace/undo", authz.ScopeRunsWrite, s.handleWorkspaceUndo)
	s.route(mux, "POST /api/sessions/{id}/truncate", authz.ScopeSessionsWrite, s.handleTruncateSession)
	s.route(mux, "POST /api/sessions/{id}/btw", authz.ScopeRunsWrite, s.handleBtwRun)
	s.route(mux, "GET /api/workspace/file", authz.ScopeWorkspaceRead, s.handleWorkspaceFile)
	s.route(mux, "GET /api/completions", authz.ScopeSessionsRead, s.handleCompletions)
	// Model presets (design §16): the consumer frontend lists the
	// operator-authored presets and applies one when the user picks it —
	// the preset's API key is copied server-side and never crosses the
	// wire. Member-scoped: choosing the model is a user action.
	s.route(mux, "GET /api/models", authz.ScopeModelsRead, s.handleListModels)
	s.route(mux, "POST /api/models/{id}/apply", authz.ScopeModelsWrite, s.handleApplyModel)
	s.route(mux, "GET /api/agent", authz.ScopeSettingsRead, s.handleGetAgent)
	s.route(mux, "POST /api/agent/permissions", authz.ScopeAgentWrite, s.handleSetPermissionMode)
	s.route(mux, "POST /api/agent/thinking", authz.ScopeAgentWrite, s.handleSetThinking)
	s.route(mux, "GET /api/quota", authz.ScopeSettingsRead, s.handleQuota)
	s.route(mux, "GET /api/claude-env", authz.ScopeSettingsRead, s.handleGetClaudeEnv)
	// Importing ~/.claude skills/MCP is user extensibility — the same trust
	// class as enabling a plugin (touching ~/.woca on the execution machine),
	// not a server-settings rewrite: members can curate their environment.
	s.route(mux, "PUT /api/claude-env", authz.ScopePluginsWrite, s.handlePutClaudeEnv)
	s.route(mux, "GET /api/plugins", authz.ScopePluginsRead, s.handleGetPlugins)
	s.route(mux, "PUT /api/plugins", authz.ScopePluginsWrite, s.handlePutPlugins)
	s.route(mux, "POST /api/plugins/{name}/install", authz.ScopePluginsWrite, s.handleInstallPlugin)
	s.route(mux, "POST /api/plugins/{name}/reinstall", authz.ScopePluginsWrite, s.handleInstallPlugin)
	s.route(mux, "GET /api/settings", authz.ScopeSettingsRead, s.handleGetSettings)
	s.route(mux, "PUT /api/settings", authz.ScopeSettingsWrite, s.handleUpdateSettings)
	// Web search: reading the config is a settings read; changing it or
	// running a probe both reach the public internet, so they carry the
	// write scope (the same posture as the sandbox verify endpoint).
	s.route(mux, "GET /api/websearch", authz.ScopeSettingsRead, s.handleGetWebSearch)
	s.route(mux, "PUT /api/websearch", authz.ScopeSettingsWrite, s.handlePutWebSearch)
	s.route(mux, "POST /api/websearch/test", authz.ScopeSettingsWrite, s.handleTestWebSearch)
	s.route(mux, "GET /api/memory", authz.ScopeMemoryRead, s.handleListMemory)
	s.route(mux, "GET /api/memory/stats", authz.ScopeMemoryRead, s.handleMemoryStats)
	s.route(mux, "GET /api/stats", authz.ScopeSettingsRead, s.handleStats)
	s.route(mux, "POST /api/memory", authz.ScopeMemoryWrite, s.handleRememberMemory)
	s.route(mux, "DELETE /api/memory/{id}", authz.ScopeMemoryWrite, s.handleForgetMemory)
	mux.Handle("/", s.staticHandler())
	localGate := gateway.RequireLocalTenant(s.hostGateExempt...)
	if s.runsOffHost {
		// Runs execute on sandboxed workers (queue dispatch), so the run
		// surface is tenant-safe; the model-preset list/apply is the
		// consumer frontend's selection surface (keys stay server-side).
		// Every other host surface stays gated.
		localGate = gateway.RequireLocalTenant(append(s.hostGateExempt, "/api/runs", "/api/models")...)
	}
	if s.nativeAuth != nil {
		h := gateway.Authenticate(s.auth, s.nativeAuth.SessionAuthenticator(), s.anonymousAPI)(
			localGate(mux),
		)
		return webfx.Chain(h, webfx.RequestID(), webfx.Logger(s.log), webfx.Recovery(s.log))
	}
	h := gateway.Authenticate(s.auth, nil, s.anonymousAPI)(
		localGate(mux),
	)
	return webfx.Chain(h, webfx.RequestID(), webfx.Logger(s.log), webfx.Recovery(s.log))
}

// streamReauthInterval bounds how long a revoked credential can keep an
// open SSE stream alive.
const streamReauthInterval = 5 * time.Second

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
		s.rootCancel() // server lifecycle over: queue-dispatched run contexts end too
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.admission.stop()
		s.rootCancel() // server lifecycle over: queue-dispatched run contexts end too
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// The grace window elapsed with a connection still stuck
			// (e.g. accepted but idle pre-request). Cancellation is a
			// clean stop, not a serve failure: force-close and report nil.
			s.log.Debug().Err(err).Msg("graceful shutdown timed out; force-closing")
			_ = srv.Close()
		}
		return nil
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{"ok": true}
	if s.grafanaURL != "" {
		payload["grafana_url"] = s.grafanaURL
	}
	writeJSON(w, http.StatusOK, payload)
}

// attachmentLimits bound prompt attachments.
const (
	maxAttachments     = 6
	maxAttachmentBytes = 8 << 20 // decoded, across all attachments
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteError(w, status, msg)
}
