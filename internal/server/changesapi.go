package server

// changesapi.go — the checkpoint/changes plane for the web UI's review
// surfaces: the Changes tab's pending-change diffs (GET
// /api/workspace/changes), workspace rollback (POST /api/workspace/undo,
// backed by tools.FileHistory) and the conversation leg of "revert code +
// conversation" (POST /api/sessions/{id}/truncate).

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/diffutil"
)

// changeFile is one pending (undoable) workspace change rendered for the
// Changes review tab.
type changeFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
	Binary  bool   `json:"binary"`
	Diff    string `json:"diff"`
}

// binaryScanLimit bounds the NUL-byte heuristic for binary detection.
const binaryScanLimit = 8192

func isBinary(b []byte) bool {
	limit := len(b)
	if limit > binaryScanLimit {
		limit = binaryScanLimit
	}
	for i := range limit {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func relTo(ws, path string) string {
	rel, err := filepath.Rel(ws, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// runWorkspaceCtx resolves the workspace from ?workspace= (falling back to
// the server workdir) and decorates the context the same way the run path
// does, so FileHistory scoping matches between runs and these endpoints.
func (s *Server) runWorkspaceCtx(r *http.Request, workspace string) (string, context.Context) {
	ws := workspace
	if ws == "" {
		ws = s.workdir
	}
	return ws, agent.WithRunWorkspace(r.Context(), ws)
}

// handleWorkspaceChanges reports every pending (undoable) change for the
// requested workspace as unified diffs, plus the undo/redo step counts.
func (s *Server) handleWorkspaceChanges(w http.ResponseWriter, r *http.Request) {
	ws, ctx := s.runWorkspaceCtx(r, r.URL.Query().Get("workspace"))
	changes := s.agent.PendingFiles(ctx)
	files := make([]changeFile, 0, len(changes))
	for _, ch := range changes {
		rel := relTo(ws, ch.Path)
		cf := changeFile{Path: rel, Created: ch.Before == nil, Deleted: ch.After == nil}
		if isBinary(ch.Before) || isBinary(ch.After) {
			cf.Binary = true
		} else {
			ops := diffutil.Diff(diffutil.SplitLines(string(ch.Before)), diffutil.SplitLines(string(ch.After)))
			cf.Added, cf.Removed = diffutil.Counts(ops)
			cf.Diff = diffutil.Unified(rel, ch.Before, ch.After, 3)
		}
		files = append(files, cf)
	}
	undoN, redoN := s.agent.FileHistoryCounts(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "undo_steps": undoN, "redo_steps": redoN,
	})
}

// handleWorkspaceUndo is checkpoint rollback: with path set it reverts to
// before that file was first touched (unwinding anything after it); with
// steps set it reverts that many changes from the top; otherwise it reverts
// everything pending.
func (s *Server) handleWorkspaceUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
		Path      string `json:"path"`
		Steps     int    `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ws, ctx := s.runWorkspaceCtx(r, req.Workspace)
	var applied int
	var summary string
	var err error
	if req.Path != "" {
		applied, summary, err = s.agent.UndoFilesThroughPath(ctx, filepath.Join(ws, req.Path))
	} else {
		n := req.Steps
		if n <= 0 {
			n, _ = s.agent.FileHistoryCounts(ctx)
		}
		applied, summary, err = s.agent.UndoFilesSteps(ctx, n)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	undoN, redoN := s.agent.FileHistoryCounts(ctx)
	s.auditEvent(r, "workspace_undo", ws)
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied, "summary": summary, "undo_steps": undoN, "redo_steps": redoN,
	})
}

// handleTruncateSession drops every message after the first `keep` (clamped
// to [0, len]) and persists the shortened transcript — the conversation leg
// of "revert code + conversation" rollback.
func (s *Server) handleTruncateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keep         int  `json:"keep"`
		DropLastTurn bool `json:"drop_last_turn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sid := r.PathValue("id")
	msgs := s.sessions.load(r.Context(), sid)
	keep := req.Keep
	if req.DropLastTurn {
		// Roll the conversation back to before the final user turn: keep
		// everything up to (not including) the last human message.
		keep = len(msgs)
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == messages.RoleHuman {
				keep = i
				break
			}
		}
	}
	if keep < 0 {
		keep = 0
	}
	if keep > len(msgs) {
		keep = len(msgs)
	}
	ws := s.sessions.workspaceOf(sid)
	if err := s.sessions.save(r.Context(), sid, msgs[:keep], ws); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditEvent(r, "session_truncate", sid)
	writeJSON(w, http.StatusOK, map[string]any{"kept": keep})
}
