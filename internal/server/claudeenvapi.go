package server

// The Claude-environment settings API: the Skill / MCP checkbox page in the
// web settings panel. GET /api/claudeenv lists what was detected under
// ~/.claude (skills) and ~/.claude.json (global mcpServers) plus the current
// selection; PUT /api/claudeenv saves a new selection, syncs the ~/.woca
// skill symlinks and mcp.json, and live-applies the skill set (MCP servers
// connect at the next agent start).

import (
	"encoding/json"
	"net/http"

	"github.com/projanvil/woca/internal/claudeenv"
)

// claudeEnvState is the GET /api/claudeenv payload.
type claudeEnvState struct {
	Skills   []claudeenv.SkillEntry `json:"skills"`
	MCP      []claudeenv.MCPServer  `json:"mcp"`
	Selected struct {
		Skills []string `json:"skills"`
		MCP    []string `json:"mcp"`
	} `json:"selected"`
}

func (s *Server) currentClaudeSelection() (skills, mcp []string) {
	if s.settings == nil {
		return nil, nil
	}
	cfg := s.settings.Load()
	return cfg.ClaudeSkills, cfg.ClaudeMCP
}

func (s *Server) handleGetClaudeEnv(w http.ResponseWriter, _ *http.Request) {
	var out claudeEnvState
	skills, err := claudeenv.Skills()
	if err != nil {
		// Detection is best-effort: a missing ~/.claude shows empty lists.
		skills = nil
	}
	out.Skills = skills
	if out.Skills == nil {
		out.Skills = []claudeenv.SkillEntry{}
	}
	servers, err := claudeenv.Servers()
	if err != nil {
		servers = nil
	}
	out.MCP = servers
	if out.MCP == nil {
		out.MCP = []claudeenv.MCPServer{}
	}
	out.Selected.Skills, out.Selected.MCP = s.currentClaudeSelection()
	if out.Selected.Skills == nil {
		out.Selected.Skills = []string{}
	}
	if out.Selected.MCP == nil {
		out.Selected.MCP = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutClaudeEnv(w http.ResponseWriter, r *http.Request) {
	if s.applyClaudeEnv == nil {
		writeError(w, http.StatusServiceUnavailable, "claude environment switching is not available in this session")
		return
	}
	var req struct {
		Skills []string `json:"skills"`
		MCP    []string `json:"mcp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applyClaudeEnv(req.Skills, req.MCP); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"skills":      req.Skills,
		"mcp":         req.MCP,
		"mcp_restart": true, // MCP servers connect at agent startup
	})
}
