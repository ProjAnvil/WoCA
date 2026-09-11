// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/slashcmds"
)

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
	_ = s.sessions.save(r.Context(), id, nil, ws)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     id,
		"workspace":      ws,
		"workspace_name": workspaceDisplayName(ws),
	})
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
	writeJSON(w, http.StatusOK, buildWorkspaceTree(s.workspaceForRequest(r)))
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
