package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/projanvil/woca/internal/memory"
)

// memoryListResponse is the GET /api/memory payload. Engine is "none" when
// the memory subsystem is disabled (graceful degradation); "ladym" is the
// backend kind label, not a per-instance description.
type memoryListResponse struct {
	Engine string       `json:"engine"`
	Items  []memory.Hit `json:"items"`
	Total  int          `json:"total"`
}

// memEngineLive resolves the memory engine at request time: the agent's
// swapped pointer first (survives live rebuilds), the construction-time
// config snapshot as the fallback.
func (s *Server) memEngineLive() memory.Engine {
	if s.agent != nil {
		return s.agent.MemoryEngine()
	}
	return s.memEngine
}

// memoryWorkspace resolves the request workspace, defaulting to the server
// workdir (mirrors startRun's default).
func (s *Server) memoryWorkspace(r *http.Request) string {
	if ws := r.URL.Query().Get("workspace"); ws != "" {
		return ws
	}
	return s.workdir
}

func (s *Server) handleListMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngineLive()
	if eng == nil {
		writeJSON(w, http.StatusOK, memoryListResponse{Engine: "none", Items: []memory.Hit{}})
		return
	}
	q := r.URL.Query()
	ws := s.memoryWorkspace(r)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	var (
		items []memory.Hit
		err   error
	)
	if query := q.Get("q"); query != "" {
		items, err = eng.Recall(r.Context(), query, ws, limit)
	} else {
		items, err = eng.List(r.Context(), ws, q.Get("layer"), limit, offset)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []memory.Hit{}
	}
	writeJSON(w, http.StatusOK, memoryListResponse{Engine: "ladym", Items: items, Total: len(items)})
}

func (s *Server) handleMemoryStats(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngineLive()
	if eng == nil {
		writeJSON(w, http.StatusOK, map[string]any{"engine": "none", "stats": nil, "workspace": s.memoryWorkspace(r)})
		return
	}
	ws := s.memoryWorkspace(r)
	st, err := eng.Stats(r.Context(), ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": "ladym", "stats": st, "workspace": ws})
}

func (s *Server) handleRememberMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngineLive()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "memory engine is disabled")
		return
	}
	var req struct {
		Workspace string   `json:"workspace"`
		Content   string   `json:"content"`
		Tags      []string `json:"tags"`
	}
	// Facts are short; cap the body at 1MiB to keep a runaway client honest.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	ws := req.Workspace
	if ws == "" {
		ws = s.workdir
	}
	tags := make([]string, 0, len(req.Tags)+1)
	tags = append(tags, req.Tags...)
	tags = append(tags, "panel")
	id, err := eng.Remember(r.Context(), ws, req.Content, tags, "woca:panel")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if id == "" {
		writeJSON(w, http.StatusOK, map[string]any{"id": "", "dropped": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleForgetMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngineLive()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "memory engine is disabled")
		return
	}
	id := r.PathValue("id")
	if err := eng.Forget(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMemoryOperations serves the recall_ops query (LadyM v0.7.0 resource
// slots): "how did I last operate on X?". The capability is embedded-engine
// only — engines without it answer 501 Not Implemented; a disabled memory
// subsystem degrades to the empty "none" shape, mirroring the list endpoint.
func (s *Server) handleMemoryOperations(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngineLive()
	if eng == nil {
		writeJSON(w, http.StatusOK, map[string]any{"engine": "none", "items": []memory.OperationHit{}})
		return
	}
	ops, ok := eng.(memory.OperationEngine)
	if !ok {
		writeError(w, http.StatusNotImplemented, "operation recall requires the embedded ladym engine")
		return
	}
	resource := r.URL.Query().Get("resource")
	if resource == "" {
		writeError(w, http.StatusBadRequest, "resource is required")
		return
	}
	ws := s.memoryWorkspace(r)
	topK, _ := strconv.Atoi(r.URL.Query().Get("top_k"))
	if topK <= 0 || topK > 100 {
		topK = 10
	}
	items, err := ops.RecallOperations(r.Context(), ws, resource, topK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []memory.OperationHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": "ladym", "workspace": ws, "items": items})
}
