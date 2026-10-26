// fsapi.go implements the /api/fs/* workspace folder-browser routes
// (product design §4.1) on top of internal/fsb: directory-only listing,
// server-confirmed mkdir, path validation, and a debounced change stream.
// Every route sits behind the gateway's scope enforcement like the rest of
// the API surface (R2: no anonymous /api access).
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/projanvil/woca/internal/fsb"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// registerFS mounts the folder-browser routes; called only when Config.FS
// is set.
func (s *Server) registerFS(mux *http.ServeMux) {
	s.route(mux, "GET /api/filesystem/list", authz.ScopeWorkspaceRead, s.handleFSList)
	s.route(mux, "GET /api/filesystem/validate", authz.ScopeWorkspaceRead, s.handleFSValidate)
	s.route(mux, "POST /api/filesystem/mkdir", authz.ScopeWorkspaceWrite, s.handleFSMkdir)
	s.route(mux, "GET /api/filesystem/watch", authz.ScopeWorkspaceRead, s.handleFSWatch)
}

// volumeFS builds the folder-browser service for the EE sandbox plane
// (Config.WorkspaceRoot set): browsing is jailed to the executor volume
// with os.OpenRoot containment — a remote tenant must never reach host
// paths through /api/filesystem/*. Called from New, replacing any
// configured service; the personal edition (empty root) keeps its own.
func volumeFS(root string) *fsb.Service {
	return fsb.New([]string{root}, false)
}

// auditEvent forwards a workspace event to the audit seam with the
// caller's identity when the bridge installed one.
func (s *Server) auditEvent(r *http.Request, action, detail string) {
	if s.audit == nil {
		return
	}
	actor := "local"
	if tc, ok := tenancy.FromContext(r.Context()); ok && tc.UserID != "" {
		actor = tc.UserID
	}
	s.audit(actor, action, detail)
}

func writeFSStatus(w http.ResponseWriter, err error, def int) int {
	switch {
	case errors.Is(err, fsb.ErrForbidden):
		// Uniform 403: out-of-jail and non-existent paths are
		// indistinguishable by design (no path oracle).
		writeError(w, http.StatusForbidden, "forbidden path")
		return http.StatusForbidden
	case errors.Is(err, fsb.ErrExists):
		writeError(w, http.StatusConflict, "directory already exists")
		return http.StatusConflict
	case errors.Is(err, fsb.ErrBadName), errors.Is(err, fsb.ErrNotDir):
		writeError(w, http.StatusBadRequest, "invalid request")
		return http.StatusBadRequest
	default:
		writeError(w, def, "fs error")
		return def
	}
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	out, err := s.fs.List(r.URL.Query().Get("path"))
	if err != nil {
		writeFSStatus(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFSValidate(w http.ResponseWriter, r *http.Request) {
	exists, isDir, writable := s.fs.Validate(r.URL.Query().Get("path"))
	writeJSON(w, http.StatusOK, map[string]bool{
		"exists": exists, "isDir": isDir, "writable": writable,
	})
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	path, err := s.fs.Mkdir(req.Parent, req.Name)
	if err != nil {
		writeFSStatus(w, err, http.StatusInternalServerError)
		return
	}
	s.auditEvent(r, "fs_mkdir", path)
	writeJSON(w, http.StatusCreated, map[string]string{"path": path})
}

// handleFSWatch streams dir-changed events (debounced in fsb) over SSE.
// Heartbeat comments keep proxies from timing the stream out.
func (s *Server) handleFSWatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	ch, err := s.fs.Watch(r.Context(), path)
	if err != nil {
		writeFSStatus(w, err, http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: ready\ndata: {\"path\":")
	_ = json.NewEncoder(w).Encode(path)
	flusher.Flush()

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, alive := <-ch:
			if !alive {
				return
			}
			fmt.Fprintf(w, "event: dir-changed\ndata: {\"path\":%q}\n\n", path)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
