// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/slashcmds"
)

// bindTimeout bounds the allocate-or-bind call: the proxy is an internal
// network hop doing a few database reads — anything slower is an outage,
// and callers fail closed rather than wait out a slow request.
const bindTimeout = 10 * time.Second

// bindClient is the allocate-or-bind HTTP client (bounded, shared).
var bindClient = &http.Client{Timeout: bindTimeout}

// bindResponseLimit bounds the bind response body (two short strings plus
// state).
const bindResponseLimit = 4 << 10

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional; empty workspace is fine
	ws := expandHomePath(req.Workspace)
	if ws == "" {
		ws = s.workdir
	}
	// EE sandbox plane (spec §3.2): an off-host session binds its workspace
	// to a sandbox workspace identity through the executor proxy's
	// allocate-or-bind API BEFORE anything persists — a failed allocation
	// fails the session closed (503, no fallback: an unbound session could
	// never run off-host). The personal edition (local tenant, no endpoint)
	// never enters this path.
	var wsID string
	if tenant, endpoint, ok := s.bindingTarget(r.Context()); ok {
		var err error
		wsID, err = s.bindWorkspace(r.Context(), endpoint, tenant, workspaceSourceKey(tenant, ws))
		if err != nil {
			s.log.Warn().Err(err).Msg("session workspace binding failed")
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	id := newSessionID()
	// Persist immediately (empty transcript + index meta): the sidebar lists
	// sessions from the persisted index, so an unpersisted session — and with
	// it a freshly added workspace group — would vanish on page refresh.
	// Before, the session first materialized when its initial run completed.
	_ = s.sessions.save(r.Context(), id, nil, ws)
	if wsID != "" {
		s.sessions.setWorkspaceID(id, wsID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     id,
		"workspace":      ws,
		"workspace_name": workspaceDisplayName(ws),
	})
}

// bindingTarget reports whether requests in this context allocate sandbox
// workspace bindings, and the tenant + executor-proxy endpoint to use: only
// the off-host EE posture with a live enabled backend endpoint, and only
// for non-local tenants (the personal edition has no tenant plane to scope
// a source key with, so it never binds).
func (s *Server) bindingTarget(ctx context.Context) (tenant, endpoint string, ok bool) {
	if !s.runsOffHost || s.sandboxEndpoint == nil {
		return "", "", false
	}
	tc, has := tenancy.FromContext(ctx)
	if !has || tc.TenantID == "" || tc.TenantID == "local" {
		return "", "", false
	}
	ep := s.sandboxEndpoint()
	if ep == "" {
		return "", "", false
	}
	return tc.TenantID, ep, true
}

// workspaceSourceKey builds the allocate-or-bind lookup key: the tenant and
// the normalized absolute workspace path. Two sessions of one tenant on one
// directory converge on one sandbox workspace; different tenants never
// share one.
func workspaceSourceKey(tenantID, workspace string) string {
	abs := workspace
	if a, err := filepath.Abs(workspace); err == nil {
		abs = a
	}
	return tenantID + "|" + abs
}

// bindWorkspace calls the executor proxy's allocate-or-bind endpoint
// (POST {endpoint}/internal/bindings, spec §3.2) and returns the stable
// sandbox workspace identity for sourceKey. Idempotent on the source key:
// concurrent callers converge on one identity. Errors are the caller's
// fail-closed signal — no local fallback identity exists.
func (s *Server) bindWorkspace(ctx context.Context, endpoint, tenantID, sourceKey string) (string, error) {
	body, err := json.Marshal(map[string]string{"tenant_id": tenantID, "source_key": sourceKey})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/internal/bindings", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("bindings: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := bindClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bindings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errOut struct {
			Error string `json:"error"`
		}
		reason := ""
		if json.NewDecoder(io.LimitReader(resp.Body, bindResponseLimit)).Decode(&errOut) == nil && errOut.Error != "" {
			reason = errOut.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("proxy returned %d", resp.StatusCode)
		}
		return "", fmt.Errorf("bindings: %s", reason)
	}
	var out struct {
		WorkspaceID string `json:"workspace_id"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bindResponseLimit)).Decode(&out); err != nil {
		return "", fmt.Errorf("bindings: %w", err)
	}
	if out.WorkspaceID == "" {
		return "", errors.New("bindings: proxy returned no workspace id")
	}
	if out.State != "" && out.State != "active" {
		s.log.Info().Str("workspace", out.WorkspaceID).Str("state", out.State).
			Msg("session bound to a non-active workspace (executor liveness gates runs, not the binding)")
	}
	return out.WorkspaceID, nil
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
	msgs := s.sessions.load(r.Context(), id)
	if msgs == nil {
		msgs = []messages.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "messages": msgs})
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	root, err := s.workspaceForRequest(r)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildWorkspaceTree(root))
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

// handleDeleteSession removes a session (transcript, goal sidecar, index
// entry). Deleting an unknown id still succeeds — the client's goal is for
// the session to be gone.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.sessions.delete(r.Context(), id)
	s.forgetSessionState(id)
	s.log.Info().Str("session", id).Msg("session deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// Errors of the EE workspace resolution chain (WorkspaceRoot set). Both are
// surfaced as 403: the caller is asking for a directory the sandbox posture
// will never serve.
var (
	// errNoBoundWorkspace: the request names no session with a bound sandbox
	// workspace — and the EE posture never falls back to host paths.
	errNoBoundWorkspace = errors.New("no session with a bound sandbox workspace")
	// errOutsideVolumeRoot: a resolved workspace escaped the executor volume
	// root (corrupted or legacy identity) — refused, never traversed.
	errOutsideVolumeRoot = errors.New("workspace path is outside the executor volume root")
)

// workspaceForRequest resolves the directory a workspace-scoped request
// targets. Personal edition (no WorkspaceRoot): the session workspace the
// client passes as ?workspace=, else the server's startup directory — the
// behavior that keeps the file sidebar in sync with the selected session's
// project in multi-workspace serve mode, unchanged. EE (WorkspaceRoot set):
// the request must name a session (?session= or ?session_id=) whose index
// meta carries a bound sandbox workspace identity, and the resolved
// directory is <WorkspaceRoot>/<id>, always inside the volume. Anything
// else is an error — a remote tenant must never reach host paths.
func (s *Server) workspaceForRequest(r *http.Request) (string, error) {
	if s.workspaceRoot == "" {
		if ws := r.URL.Query().Get("workspace"); ws != "" {
			return ws, nil
		}
		return s.workdir, nil
	}
	sid := r.URL.Query().Get("session")
	if sid == "" {
		sid = r.URL.Query().Get("session_id")
	}
	wsID := ""
	if sid != "" {
		wsID = s.sessions.workspaceIDOf(sid)
	}
	if wsID == "" {
		return "", errNoBoundWorkspace
	}
	p := filepath.Join(s.workspaceRoot, wsID)
	// Defense in depth: the identity is store-minted, but a corrupted or
	// hand-edited value carrying separators must never traverse out of the
	// volume root.
	if !pathWithinRoot(s.workspaceRoot, p) {
		return "", errOutsideVolumeRoot
	}
	return p, nil
}

// pathWithinRoot reports whether p is root or inside it (both made
// absolute; lexical containment).
func pathWithinRoot(root, p string) bool {
	absRoot, err1 := filepath.Abs(root)
	absP, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	return absP == absRoot || strings.HasPrefix(absP, absRoot+string(filepath.Separator))
}

// handleWorkspaceFile returns a single workspace file's content for the
// right-sidebar preview (bounded, text files only).
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	root, err := s.workspaceForRequest(r)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	node, err := readWorkspaceFile(root, r.URL.Query().Get("path"))
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
