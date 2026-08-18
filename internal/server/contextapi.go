package server

// GET /api/sessions/{id}/context — the context-usage snapshot for the chat
// input's context indicator: an estimated per-category breakdown of what the
// next model request would carry (system prompt / memory files / tool
// schemas / history) plus the provider-reported usage of the most recent
// model call. Computed on demand (hover) in memory; no model round-trip.

import (
	"net/http"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
)

func (s *Server) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}
	id := r.PathValue("id")
	history := s.sessions.load(id)
	ws := s.sessions.workspaceOf(id)
	if ws == "" {
		ws = s.workdir
	}
	// Rebuild the per-run workspace context the system prompt and memory
	// files are resolved against (same decoration startRun applies).
	ctx := agent.WithRunWorkspace(r.Context(), ws)
	info := s.agent.ContextBreakdown(ctx, history)
	// The live config's model name is authoritative (it refreshes with
	// SetModel); the agent-side name is a fallback for embedders without a
	// cfg hook.
	cfg := s.liveConfig()
	if cfg.Model != "" {
		info.Model = cfg.Model
	}
	info.ContextWindow = config.ContextWindowFor(info.Model)
	writeJSON(w, http.StatusOK, info)
}
