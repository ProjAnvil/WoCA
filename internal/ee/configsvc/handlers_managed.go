// Package configsvc is the standalone settings management plane (design
// §16): a dedicated role that owns configuration read/write, validation,
// export/import, and its console SPA. Business roles (woca-scheduler,
// woca-worker) keep reading the shared settings tables directly — the
// config service is the sole management entry, deployed independently
// behind the gateway's admin port.
//
// Product design v1.8: this console is focused on woca's own settings
// domains (models, storage, memory, dispatch). Tenant and bridge
// governance — credentials, sessions, enrollments, the registration
// inventory, the governance audit — lives on the service plane (the
// tenant ops console and the core identity service). The config package
// carries NO user-identity material: bridges hold zero user credentials
// and validate against the core online.
package configsvc

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/projanvil/woca/internal/wocadb"
)

func (s *Service) handleListManagedSkills(w http.ResponseWriter, _ *http.Request) {
	skillsList, err := s.managedSkillsPayload()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "skill inventory unavailable")
		return
	}
	if skillsList == nil {
		skillsList = []ManagedSkill{}
	}
	writeJSON(w, http.StatusOK, skillsList)
}

func (s *Service) handlePutManagedSkill(w http.ResponseWriter, r *http.Request) {
	var req ManagedSkill
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}
	if len(req.Name) > 64 || len(req.Body) > 128<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name ≤64 chars, body ≤128KiB"})
		return
	}
	now := wocadb.Now()
	if _, err := s.db.Exec(
		`INSERT INTO managed_skills (name, description, body, updated_at) VALUES (`+phI(s.db, 0)+`, `+phI(s.db, 1)+`, `+phI(s.db, 2)+`, `+phI(s.db, 3)+`)
		 ON CONFLICT(name) DO UPDATE SET description = excluded.description, body = excluded.body, updated_at = excluded.updated_at`,
		req.Name, req.Description, req.Body, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.log.Info().Str("skill", req.Name).Msg("managed skill saved via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleDeleteManagedSkill(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`DELETE FROM managed_skills WHERE name = `+phI(s.db, 0), r.PathValue("name"))
	if err != nil || nRows(res) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown skill"})
		return
	}
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleListManagedMCP(w http.ResponseWriter, _ *http.Request) {
	servers, err := s.managedMCPPayload()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mcp inventory unavailable")
		return
	}
	if servers == nil {
		servers = []ManagedMCP{}
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Service) handlePutManagedMCP(w http.ResponseWriter, r *http.Request) {
	var req ManagedMCP
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Spec = strings.TrimSpace(req.Spec)
	if req.Name == "" || req.Spec == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and spec are required"})
		return
	}
	// The spec grammar the bridges consume: name=command (stdio) or
	// name=http(s)://endpoint (streamable HTTP). The stored spec must
	// carry its own name prefix or start with it — normalize to
	// name=<rest>.
	rest := strings.TrimSpace(strings.TrimPrefix(req.Spec, req.Name+"="))
	if rest == "" || (!strings.HasPrefix(rest, "http://") && !strings.HasPrefix(rest, "https://") && !strings.Contains(rest, " ")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "spec must be a command or an http(s):// endpoint"})
		return
	}
	now := wocadb.Now()
	if _, err := s.db.Exec(
		`INSERT INTO managed_mcp (name, spec, updated_at) VALUES (`+phI(s.db, 0)+`, `+phI(s.db, 1)+`, `+phI(s.db, 2)+`)
		 ON CONFLICT(name) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`,
		req.Name, req.Name+"="+rest, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.log.Info().Str("mcp", req.Name).Msg("managed mcp saved via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleDeleteManagedMCP(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Exec(`DELETE FROM managed_mcp WHERE name = `+phI(s.db, 0), r.PathValue("name"))
	if err != nil || nRows(res) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown mcp server"})
		return
	}
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
