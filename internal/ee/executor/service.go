package executor

// The executor's contract v2 sandbox HTTP service (internal/platform/
// executor/CONTRACT.md): all 16 endpoints plus one internal ops endpoint,
// served entirely through the narrow Backend interface, so the same service
// runs over the FakeBackend in tests and over the docker backend in
// production. Workspace-routed sandboxes hold their workspace's DirLock
// from create to destroy and upload/sync through the WorkspaceStore.
//
// File-surface endpoints are built from the generic Backend's path-mapping
// primitives: read/stat/glob/grep stream a CopyOut tar, write/mkdir pack a
// one-entry CopyIn tar. This keeps every container-side path resolution
// inside the Backend (no host-path assumptions in the service). Exec argv
// is passed through verbatim — commands run inside the container and see
// container paths.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	nacosrt "github.com/projanvil/woca/internal/ee/nacosrt"
)

// Label keys carried by sandbox containers (design §10.3). Create passes the
// Labels hook's map through verbatim — the shape (which of these keys, with
// what values) is the caller's; /internal/sandboxes filters with them.
const (
	// LabelSandbox marks a container as a woca sandbox ("1"); the GC and
	// the internal listing filter on it.
	LabelSandbox = "woca.sandbox"
	// LabelRun is the owning run's id, when known.
	LabelRun = "woca.run"
	// LabelWorkspace is the workspace the sandbox is routed under.
	LabelWorkspace = "woca.workspace"
)

// ServiceConfig configures the contract v2 sandbox service.
type ServiceConfig struct {
	// Backend provisions and lists sandbox containers (required).
	Backend Backend
	// Store owns the host-side workspace directories; workspace-routed
	// sandboxes upload from and sync into it. Nil rejects workspace
	// routing explicitly.
	Store *WorkspaceStore
	// MaxConcurrent caps concurrent live sandboxes; 0 means unlimited. A
	// create past the cap answers 503 (design §3.3 rule 4).
	MaxConcurrent int
	// MaxFileBytes caps how many bytes of any single file the
	// file-surface endpoints (read/grep/stat) may pull through CopyOut;
	// a larger file answers 413 naming the file and the cap. Zero or
	// negative defaults to 8 MiB — the same bound as the session output
	// buffer. Glob and archive downloads stream headers/bytes through
	// without buffering, so they are not subject to the per-file cap.
	MaxFileBytes int64
	// Logger receives lifecycle and error logs. The zero value disables
	// logging.
	Logger zerolog.Logger
	// Hardening selects the security posture containers are created with.
	Hardening Hardening
	// Options supplies the live backend execution parameters (image,
	// runtime, network — nacosrt BackendOptions) per create; nil means
	// "no options" and leaves the fields to the backend's judgment.
	Options func() nacosrt.BackendOptions
	// Labels returns the container labels for a sandbox created under the
	// given identity — the workspace/run id the executor knows at create
	// time. The service passes the map through into CreateSpec.Labels.
	Labels func(runID string) map[string]string
	// ActiveRuns is a passive hook reporting the executor-side run count
	// for the ack heartbeat's Load figure (design §10.1); a nil hook means
	// the heartbeat reports 0. It is deliberately NOT consulted for
	// admission: it counts runs, while MaxConcurrent caps sandboxes — a
	// healthy executor with MaxConcurrent live runs and zero sandboxes
	// must still accept creates.
	ActiveRuns func() int
	// APIKey enables bearer authentication on every endpoint (contract
	// §1) when non-empty. Empty means explicitly unauthenticated — a WARN
	// is logged at startup — on the stated boundary that the executor
	// trusts its fronting network (design §9.2: the key is defense in
	// depth, not the perimeter).
	APIKey string
}

// Service is the executor sandbox service: the 16 contract endpoints plus
// GET /internal/sandboxes, over one Backend.
type Service struct {
	cfg ServiceConfig
	log zerolog.Logger

	mu        sync.Mutex
	sandboxes map[string]*sandbox

	handler   http.Handler
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewService builds the sandbox service, starts the idle-session reaper,
// and returns it as an http.Handler. The concrete *Service also exposes
// ActiveRuns and Close for the executor entrypoint.
func NewService(cfg ServiceConfig) http.Handler {
	log := cfg.Logger // zerolog's zero value is a valid no-op logger
	if cfg.APIKey == "" {
		log.Warn().Msg("sandbox service authentication is disabled (empty API key); relying on network isolation")
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 8 << 20
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg:       cfg,
		log:       log,
		sandboxes: map[string]*sandbox{},
		ctx:       ctx,
		cancel:    cancel,
	}
	s.handler = s.withAuth(s.routes())
	s.wg.Add(1)
	go s.reapSessions()
	return s
}

var _ http.Handler = (*Service)(nil)

// ServeHTTP implements http.Handler.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// ActiveRuns reports the number of live sandboxes in the registry — the
// admission counter for MaxConcurrent, and the figure an executor may
// report as its ack Load when it has no separate run count.
func (s *Service) ActiveRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sandboxes)
}

// Close stops the reaper and every session pump, force-removes every
// container, and releases every workspace lock. Idempotent; returns the
// first teardown error encountered (containers gone are fine, containers
// that refuse removal are not — the labeled GC catches those later).
func (s *Service) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
		s.mu.Lock()
		live := make([]*sandbox, 0, len(s.sandboxes))
		for _, sbx := range s.sandboxes {
			live = append(live, sbx)
		}
		s.sandboxes = map[string]*sandbox{}
		s.mu.Unlock()
		for _, sbx := range live {
			if err := sbx.destroy(context.Background(), s.log); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// --- routing and plumbing ----------------------------------------------------

func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/create", s.handleCreate)
	mux.HandleFunc("POST /sandbox/{id}/exec", s.withSandbox(s.handleExec))
	mux.HandleFunc("POST /sandbox/{id}/start", s.withSandbox(s.handleStart))
	mux.HandleFunc("GET /sandbox/{id}/sessions/{sid}/output", s.withSession(s.handleOutput))
	mux.HandleFunc("POST /sandbox/{id}/sessions/{sid}/input", s.withSession(s.handleInput))
	mux.HandleFunc("POST /sandbox/{id}/sessions/{sid}/kill", s.withSession(s.handleKill))
	mux.HandleFunc("GET /sandbox/{id}/read", s.withSandbox(s.handleRead))
	mux.HandleFunc("POST /sandbox/{id}/write", s.withSandbox(s.handleWrite))
	mux.HandleFunc("GET /sandbox/{id}/stat", s.withSandbox(s.handleStat))
	mux.HandleFunc("POST /sandbox/{id}/mkdir", s.withSandbox(s.handleMkdir))
	mux.HandleFunc("POST /sandbox/{id}/glob", s.withSandbox(s.handleGlob))
	mux.HandleFunc("POST /sandbox/{id}/grep", s.withSandbox(s.handleGrep))
	mux.HandleFunc("PUT /sandbox/{id}/archive", s.withSandbox(s.handlePutArchive))
	mux.HandleFunc("GET /sandbox/{id}/archive", s.withSandbox(s.handleGetArchive))
	mux.HandleFunc("POST /sandbox/{id}/workspace/sync", s.withSandbox(s.handleWorkspaceSync))
	mux.HandleFunc("DELETE /sandbox/{id}", s.handleDestroy) // idempotent 2xx per §3.15
	mux.HandleFunc("GET /internal/sandboxes", s.handleInternalSandboxes)
	return mux
}

// withAuth enforces the bearer token on every endpoint when configured.
func (s *Service) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := s.cfg.APIKey; key != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				writeErr(w, http.StatusUnauthorized, "invalid or missing bearer token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type sandboxHandler func(sbx *sandbox, w http.ResponseWriter, r *http.Request)

func (s *Service) withSandbox(h sandboxHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sbx := s.lookup(r.PathValue("id"))
		if sbx == nil {
			writeErr(w, http.StatusNotFound, "unknown sandbox "+r.PathValue("id"))
			return
		}
		h(sbx, w, r)
	}
}

type sessionHandler func(sbx *sandbox, ses *procSession, w http.ResponseWriter, r *http.Request)

func (s *Service) withSession(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sbx := s.lookup(r.PathValue("id"))
		if sbx == nil {
			writeErr(w, http.StatusNotFound, "unknown sandbox "+r.PathValue("id"))
			return
		}
		ses := sbx.session(r.PathValue("sid"))
		if ses == nil {
			writeErr(w, http.StatusNotFound, "unknown session "+r.PathValue("sid"))
			return
		}
		h(sbx, ses, w, r)
	}
}

func (s *Service) lookup(id string) *sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxes[id]
}

// isRemovedErr reports whether err is (wraps) the backend's "already
// removed" sentinel.
func isRemovedErr(err error) bool { return errors.Is(err, ErrRemoved) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeBody decodes the request body into v; an empty body passes with all
// defaults (contract endpoints tolerate advisory bodies), malformed JSON is
// a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

// absolutePath validates a container-side path: present and absolute
// (contract §3.8: "absolute path inside the sandbox").
func absolutePath(w http.ResponseWriter, p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("path %q must be an absolute container path", p))
		return false
	}
	return true
}

// envPairs converts the contract's env object into sorted "K=V" entries.
func envPairs(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, k+"="+m[k])
	}
	return out
}

// --- lifecycle -----------------------------------------------------------------

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string `json:"workspace"`
		Cwd       string `json:"cwd"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	workspace := body.Workspace
	if workspace == "" {
		// §1: when routed, the workspace identity also rides the header.
		workspace = r.Header.Get("X-Woca-Workspace")
	}
	if workspace != "" && !ValidWorkspaceID(workspace) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid workspace id %q", workspace))
		return
	}

	// Admission (§3.3 rule 4): this service's live sandboxes against the
	// configured cap. The cfg.ActiveRuns hook is a different unit (runs
	// for the ack heartbeat) and never gates this check.
	if s.cfg.MaxConcurrent > 0 && s.ActiveRuns() >= s.cfg.MaxConcurrent {
		writeErr(w, http.StatusServiceUnavailable, "sandbox service at capacity")
		return
	}

	// The workspace lock is taken before anything is created and held
	// until destroy (design §4): two sandboxes may never share a host
	// workspace. Busy is 503 — the workspace is not wedged, just taken.
	var lock *DirLock
	if workspace != "" {
		if s.cfg.Store == nil {
			writeErr(w, http.StatusInternalServerError, "workspace routing requires a workspace store")
			return
		}
		dir, err := s.cfg.Store.Dir(workspace)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		lock, err = LockDir(dir)
		if err != nil {
			if errors.Is(err, ErrWorkspaceBusy) {
				writeErr(w, http.StatusServiceUnavailable, "workspace "+workspace+" is busy (another sandbox holds it)")
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	spec := CreateSpec{Hardening: s.cfg.Hardening}
	if s.cfg.Options != nil {
		opts := s.cfg.Options()
		if err := opts.Normalize(); err != nil {
			_ = lock.Close()
			writeErr(w, http.StatusInternalServerError, "backend options: "+err.Error())
			return
		}
		spec.Image, spec.Runtime, spec.Network = opts.Image, opts.Runtime, opts.Network
	}
	if s.cfg.Labels != nil {
		spec.Labels = s.cfg.Labels(workspace)
	}
	spec.WorkDir = body.Cwd
	if spec.WorkDir == "" {
		spec.WorkDir = sandboxWorkspacePath
	}

	c, err := s.cfg.Backend.Create(r.Context(), spec)
	if err != nil {
		_ = lock.Close()
		writeErr(w, http.StatusInternalServerError, "create container: "+err.Error())
		return
	}
	if workspace != "" {
		if err := s.cfg.Store.Upload(r.Context(), c, workspace); err != nil {
			// Spec §10.2: upload failure fails the create; the container
			// dies, the host directory stays untouched, nothing stays
			// locked. destroy's idempotent semantics apply to the
			// already-registered-less container.
			_ = c.Remove(context.Background(), true)
			_ = lock.Close()
			writeErr(w, http.StatusInternalServerError, "workspace upload: "+err.Error())
			return
		}
	}

	sbx := &sandbox{id: c.ID(), container: c, workspace: workspace, lock: lock}
	s.mu.Lock()
	s.sandboxes[sbx.id] = sbx
	s.mu.Unlock()
	s.log.Info().Str("sandbox", sbx.id).Str("workspace", workspace).Msg("sandbox created")
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": sbx.id})
}

func (s *Service) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sbx := s.sandboxes[id]
	delete(s.sandboxes, id)
	s.mu.Unlock()
	if sbx == nil {
		w.WriteHeader(http.StatusNoContent) // idempotent per §3.15
		return
	}
	if err := sbx.destroy(r.Context(), s.log); err != nil {
		s.log.Error().Err(err).Str("sandbox", id).Msg("sandbox destroy failed")
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info().Str("sandbox", id).Msg("sandbox destroyed")
	w.WriteHeader(http.StatusNoContent)
}

// --- process surface --------------------------------------------------------------

func (s *Service) handleExec(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Argv      []string          `json:"argv"`
		Cwd       string            `json:"cwd"`
		Env       map[string]string `json:"env"`
		Stdin     string            `json:"stdin"`
		TimeoutMs int64             `json:"timeout_ms"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "argv must not be empty")
		return
	}
	stdin, err := base64.StdEncoding.DecodeString(body.Stdin)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "stdin not base64")
		return
	}
	spec := ExecSpec{Cmd: body.Argv, Dir: body.Cwd, Env: envPairs(body.Env), Stdin: stdin}
	if body.TimeoutMs > 0 {
		// The backend kills the command at the deadline (design §10.2
		// "执行超时": kill the process, fail the run).
		spec.Timeout = time.Duration(body.TimeoutMs) * time.Millisecond
	}
	res, err := sbx.container.Exec(r.Context(), spec)
	if err != nil {
		s.log.Error().Err(err).Str("sandbox", sbx.id).Strs("argv", body.Argv).Msg("exec failed")
		writeErr(w, http.StatusInternalServerError, "exec: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": res.ExitCode,
		"stdout":    base64.StdEncoding.EncodeToString(res.Stdout),
		"stderr":    base64.StdEncoding.EncodeToString(res.Stderr),
	})
}

func (s *Service) handleStart(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Argv []string          `json:"argv"`
		Cwd  string            `json:"cwd"`
		Env  map[string]string `json:"env"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "argv must not be empty")
		return
	}
	// The process outlives the request: it runs under the service's
	// lifetime context (canceled by Close), never the request's.
	proc, err := sbx.container.Start(s.ctx, ExecSpec{Cmd: body.Argv, Dir: body.Cwd, Env: envPairs(body.Env)})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "start: "+err.Error())
		return
	}
	sid, ses := sbx.addSession(proc)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ses.pump(s.ctx)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sid})
}

func (s *Service) handleOutput(_ *sandbox, ses *procSession, w http.ResponseWriter, r *http.Request) {
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad offset "+strconv.Quote(v))
			return
		}
		if n > 0 {
			offset = n
		}
	}
	ses.touch()
	events, next, done, exit := ses.snapshot(offset)
	type event struct {
		Stream string `json:"stream"`
		Data   string `json:"data"`
	}
	out := struct {
		Events     []event `json:"events"`
		Done       bool    `json:"done"`
		ExitCode   int     `json:"exit_code"`
		NextOffset int     `json:"next_offset"`
	}{
		Events:     make([]event, 0, len(events)),
		Done:       done,
		ExitCode:   exit,
		NextOffset: next,
	}
	for _, e := range events {
		out.Events = append(out.Events, event{Stream: e.Stream, Data: base64.StdEncoding.EncodeToString(e.Data)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleInput(_ *sandbox, ses *procSession, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data string `json:"data"`
		EOF  bool   `json:"eof"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ses.touch()
	if body.EOF {
		if err := ses.proc.CloseStdin(); err != nil && !ses.isDone() {
			writeErr(w, http.StatusInternalServerError, "stdin close: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "input data not base64")
		return
	}
	if ses.isDone() {
		w.WriteHeader(http.StatusNoContent) // no-op after exit per §3.5
		return
	}
	if err := ses.proc.WriteStdin(data); err != nil {
		if ses.isDone() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusInternalServerError, "stdin write: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleKill(_ *sandbox, ses *procSession, w http.ResponseWriter, r *http.Request) {
	ses.touch()
	if err := ses.proc.Kill(r.Context()); err != nil && !ses.isDone() {
		writeErr(w, http.StatusInternalServerError, "kill: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent) // killing an exited session is a no-op per §3.6
}

// --- file surface -------------------------------------------------------------------

// copyOutHeader streams CopyOut(p) just far enough to return the first
// tar entry's header, with the reader positioned at that entry's data.
// The returned close function is always non-nil (a no-op on error), so
// callers may defer it unconditionally; they read at most what they need,
// which is what bounds memory on the file surface.
func copyOutHeader(ctx context.Context, c Container, p string) (*tar.Header, *tar.Reader, func(), error) {
	rc, err := c.CopyOut(ctx, p)
	if err != nil {
		return nil, nil, func() {}, err
	}
	closeFn := func() { _ = rc.Close() }
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		closeFn()
		return nil, nil, func() {}, fmt.Errorf("copy out %s: no entry: %w", p, err)
	}
	return hdr, tr, closeFn, nil
}

// fileTooLarge writes the 413 error body naming the offending file and the
// configured cap.
func (s *Service) fileTooLarge(w http.ResponseWriter, p string, size int64) {
	writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
		"%s: %d bytes exceeds the %d-byte per-file cap", p, size, s.cfg.MaxFileBytes))
}

func (s *Service) handleRead(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !absolutePath(w, p) {
		return
	}
	hdr, tr, closeFn, err := copyOutHeader(r.Context(), sbx.container, p)
	defer closeFn()
	if err != nil {
		writeCopyOutErr(w, err, "read")
		return
	}
	if hdr.Typeflag == tar.TypeDir {
		writeErr(w, http.StatusBadRequest, "path "+p+" is a directory")
		return
	}
	// The entry size is known from the header: refuse over-cap files
	// before any of their bytes are read.
	if hdr.Size > s.cfg.MaxFileBytes {
		s.fileTooLarge(w, p, hdr.Size)
		return
	}
	data, err := io.ReadAll(io.LimitReader(tr, s.cfg.MaxFileBytes+1))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString(data)})
}

func (s *Service) handleStat(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !absolutePath(w, p) {
		return
	}
	// Metadata only: the header is read, the entry data never is.
	hdr, _, closeFn, err := copyOutHeader(r.Context(), sbx.container, p)
	defer closeFn()
	if err != nil {
		writeCopyOutErr(w, err, "stat")
		return
	}
	if hdr.Typeflag != tar.TypeDir && hdr.Size > s.cfg.MaxFileBytes {
		s.fileTooLarge(w, p, hdr.Size)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":     hdr.Mode,
		"size":     hdr.Size,
		"mtime_ms": hdr.ModTime.UnixMilli(),
		"is_dir":   hdr.Typeflag == tar.TypeDir,
	})
}

func (s *Service) handleWrite(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Mode    int64  `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !absolutePath(w, body.Path) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "content not base64")
		return
	}
	clean := path.Clean(body.Path)
	base := path.Base(clean)
	if base == "/" || base == "." {
		writeErr(w, http.StatusBadRequest, "path "+body.Path+" does not name a file")
		return
	}
	mode := body.Mode
	if mode == 0 {
		mode = 0o644 // §3.8: omitted/0 → backend default; pick it here
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: base, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		writeErr(w, http.StatusInternalServerError, "build archive: "+err.Error())
		return
	}
	if _, err := tw.Write(data); err != nil {
		writeErr(w, http.StatusInternalServerError, "build archive: "+err.Error())
		return
	}
	if err := tw.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "build archive: "+err.Error())
		return
	}
	// CopyIn creates missing parent directories for entries.
	if err := sbx.container.CopyIn(r.Context(), path.Dir(clean), &buf); err != nil {
		writeErr(w, http.StatusInternalServerError, "write "+body.Path+": "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleMkdir(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !absolutePath(w, body.Path) {
		return
	}
	clean := path.Clean(body.Path)
	base := path.Base(clean)
	if base == "/" || base == "." {
		writeErr(w, http.StatusBadRequest, "path "+body.Path+" does not name a directory")
		return
	}
	// A directory tar entry expands with MkdirAll semantics on both sides
	// of the Backend interface (missing parents are created).
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: base, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		writeErr(w, http.StatusInternalServerError, "build archive: "+err.Error())
		return
	}
	if err := tw.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "build archive: "+err.Error())
		return
	}
	if err := sbx.container.CopyIn(r.Context(), path.Dir(clean), &buf); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir "+body.Path+": "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// globRoot extracts a glob pattern's literal directory prefix — the
// shallowest directory that can contain any match (e.g. "/w/*/x" → "/w").
func globRoot(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*', '?', '[':
			return path.Clean(pattern[:i])
		}
	}
	return path.Clean(pattern)
}

func (s *Service) handleGlob(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pattern string `json:"pattern"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	matches := []string{}
	if body.Pattern == "" {
		writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
		return
	}
	// Enumerate the subtree under the pattern's literal prefix and match
	// with Go path.Match shape (§3.11). A missing root matches nothing.
	root := globRoot(body.Pattern)
	rc, err := sbx.container.CopyOut(r.Context(), root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
			return
		}
		writeErr(w, http.StatusInternalServerError, "glob: "+err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	top := path.Base(path.Clean("/" + root))
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "glob: read tree: "+err.Error())
			return
		}
		name := path.Clean(hdr.Name)
		candidate := root // the copied path itself
		if name != top {
			candidate = path.Join(root, strings.TrimPrefix(name, top+"/"))
		}
		if ok, _ := path.Match(body.Pattern, candidate); ok {
			matches = append(matches, candidate)
		}
	}
	slices.Sort(matches)
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

func (s *Service) handleGrep(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root       string `json:"root"`
		Pattern    string `json:"pattern"`
		MaxMatches int    `json:"max_matches"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !absolutePath(w, body.Root) {
		return
	}
	re, err := regexp.Compile(body.Pattern)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad pattern: "+err.Error())
		return
	}
	if body.MaxMatches <= 0 {
		body.MaxMatches = 200 // the standard client always sends 200 (§3.12)
	}

	rc, err := sbx.container.CopyOut(r.Context(), body.Root)
	if err != nil {
		writeCopyOutErr(w, err, "grep")
		return
	}
	defer func() { _ = rc.Close() }()

	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := make([]match, 0, body.MaxMatches)
	top := path.Base(path.Clean(body.Root))
	tr := tar.NewReader(rc)
	for len(matches) < body.MaxMatches {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "grep: read tree: "+err.Error())
			return
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // directories and symlinks carry no lines (§3.12)
		}
		name := path.Clean(hdr.Name)
		rel := strings.TrimPrefix(name, top+"/")
		if rel == name || hiddenSegments(rel) {
			continue
		}
		// Refuse over-cap files from the header size — before reading a
		// single byte of the entry.
		if hdr.Size > s.cfg.MaxFileBytes {
			s.fileTooLarge(w, body.Root+"/"+rel, hdr.Size)
			return
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "grep: read file: "+err.Error())
			return
		}
		for i, line := range strings.Split(string(data), "\n") {
			if len(matches) >= body.MaxMatches {
				break
			}
			if re.MatchString(line) {
				matches = append(matches, match{
					Path: body.Root + "/" + rel,
					Line: i + 1,
					Text: base64.StdEncoding.EncodeToString([]byte(line)),
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// hiddenSegments reports whether any path segment starts with "." — the
// contract's hidden-directory skip, applied below the grep root.
func hiddenSegments(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func writeCopyOutErr(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, http.StatusNotFound, op+": no such path")
		return
	}
	writeErr(w, http.StatusInternalServerError, op+": "+err.Error())
}

// --- archive surface ------------------------------------------------------------------

func (s *Service) handlePutArchive(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	// CopyIn accepts plain and gzipped tars; garbage is its error to report.
	if err := sbx.container.CopyIn(r.Context(), sandboxWorkspacePath, r.Body); err != nil {
		writeErr(w, http.StatusBadRequest, "archive: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleGetArchive(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	rc, err := sbx.container.CopyOut(r.Context(), sandboxWorkspacePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			rc = io.NopCloser(bytes.NewReader(nil)) // empty workspace = empty archive
		} else {
			writeErr(w, http.StatusInternalServerError, "archive: "+err.Error())
			return
		}
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	zw := gzip.NewWriter(w)
	// CopyOut names entries under the copied path's base name (Docker
	// archive convention); the contract (§4) wants entries relative to the
	// workspace root, so re-root the stream while copying.
	top := path.Base(sandboxWorkspacePath)
	tr := tar.NewReader(rc)
	tw := tar.NewWriter(zw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.log.Error().Err(err).Str("sandbox", sbx.id).Msg("archive stream failed")
			return
		}
		name := path.Clean(hdr.Name)
		if name == top {
			continue
		}
		rel := strings.TrimPrefix(name, top+"/")
		if rel == "" {
			continue
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return
		}
		if hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return
			}
		}
	}
	_ = tw.Close()
	_ = zw.Close()
}

// --- workspace sync --------------------------------------------------------------------

func (s *Service) handleWorkspaceSync(sbx *sandbox, w http.ResponseWriter, r *http.Request) {
	if sbx.workspace == "" {
		writeErr(w, http.StatusBadRequest, "sandbox is not workspace-routed")
		return
	}
	if s.cfg.Store == nil {
		writeErr(w, http.StatusInternalServerError, "workspace store not configured")
		return
	}
	// Sync extracts the container /workspace and atomically swaps the host
	// directory (spec §10.2: a failed sync fails the run, the host keeps
	// the old tree).
	if err := s.cfg.Store.Sync(r.Context(), sbx.container, sbx.workspace); err != nil {
		s.log.Error().Err(err).Str("sandbox", sbx.id).Str("workspace", sbx.workspace).Msg("workspace sync failed")
		writeErr(w, http.StatusInternalServerError, "workspace sync: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- internal ops endpoint ---------------------------------------------------------------

// handleInternalSandboxes serves GET /internal/sandboxes?run=<id>&workspace=<id>:
// the executor's ops surface (not part of contract v2) listing live sandbox
// containers by label — the demo's "no leftover containers" assertion and
// the labeled GC's discovery both build on it (design §10.3, §11.2).
func (s *Service) handleInternalSandboxes(w http.ResponseWriter, r *http.Request) {
	filter := map[string]string{LabelSandbox: "1"}
	if v := r.URL.Query().Get("run"); v != "" {
		filter[LabelRun] = v
	}
	if v := r.URL.Query().Get("workspace"); v != "" {
		filter[LabelWorkspace] = v
	}
	containers, err := s.cfg.Backend.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list sandboxes: "+err.Error())
		return
	}
	type entry struct {
		ID      string            `json:"id"`
		Running bool              `json:"running"`
		Labels  map[string]string `json:"labels"`
	}
	out := struct {
		Sandboxes []entry `json:"sandboxes"`
	}{Sandboxes: make([]entry, 0, len(containers))}
	for _, c := range containers {
		st, err := c.State(r.Context())
		if err != nil {
			continue // a container that cannot be stated is being torn down
		}
		out.Sandboxes = append(out.Sandboxes, entry{ID: c.ID(), Running: st.Running, Labels: st.Labels})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- session reaper ------------------------------------------------------------------------

// reapSessions closes sessions nobody has touched for the idle TTL, so a
// client that leaks a session cannot keep its process alive forever
// (contract §5's server-side TTL safety net).
func (s *Service) reapSessions() {
	defer s.wg.Done()
	iv := sessionIdleTTL / 3
	if iv > time.Second {
		iv = time.Second
	}
	if iv < 20*time.Millisecond {
		iv = 20 * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			live := make([]*sandbox, 0, len(s.sandboxes))
			for _, sbx := range s.sandboxes {
				live = append(live, sbx)
			}
			s.mu.Unlock()
			for _, sbx := range live {
				sbx.reapIdle(now, sessionIdleTTL)
			}
		}
	}
}
