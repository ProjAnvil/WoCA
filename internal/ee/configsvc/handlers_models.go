// Package configsvc is the standalone settings management plane (design
// §16): a dedicated role that owns configuration read/write, validation,
// export/import, and its console SPA. Business roles (woca-server,
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

	"github.com/projanvil/woca/internal/wocadb"
)

// handleGWListModels serves the preset catalog to enrolled bridges —
// the fields the selection surface needs, never credentials: a preset's
// API key travels only inside the verify authorization (below).
func (s *Service) handleGWListModels(w http.ResponseWriter, _ *http.Request) {
	presets, err := s.db.ListModelPresets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	out := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "provider": p.Provider, "model": p.Model,
			"max_tokens": p.MaxTokens, "temperature": p.Temperature, "base_url": p.BaseURL,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGWVerifyModel is the run-time authorization (product ruling):
// the bridge presents the preferred model (preset id, or the
// provider+model pair); the config plane decides whether that model
// exists in the operator-authored catalog. Verified models return the
// full preset INCLUDING its API key — credential material that rides
// only this machine-authenticated channel and never any client surface
// (D5/R4). Unknown models are refused; nothing is hinted back.
func (s *Service) handleGWVerifyModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	presets, err := s.db.ListModelPresets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	for _, p := range presets {
		if (req.ID != "" && p.ID == req.ID) ||
			(req.ID == "" && req.Provider != "" && p.Provider == req.Provider && p.Model == req.Model) {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": p.ID, "name": p.Name, "provider": p.Provider, "model": p.Model,
				"max_tokens": p.MaxTokens, "temperature": p.Temperature, "base_url": p.BaseURL,
				"api_key": s.settings.GetSecret(wocadb.ModelPresetKeySecret(p.ID)),
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown model"})
}

// modelPayload is one preset over the wire. KeySet reports whether the
// encrypted key secret exists (the key itself never leaves the store).
type modelPayload struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	MaxTokens   int64   `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	BaseURL     string  `json:"base_url"`
	KeySet      bool    `json:"key_set"`
	UpdatedAt   int64   `json:"updated_at"`
}
