package server

// The built-in plugin management API backing the settings panel's plugin
// section. GET /api/plugins lists the catalog (manifests embedded in the
// binary) with enabled flags and runtime install state; PUT /api/plugins
// persists the enabled selection and rebinds the agent's tools live;
// POST /api/plugins/{name}/install starts an async runtime install whose
// progress is visible through GET polling.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/settings"
)

// pluginsResponse is the GET /api/plugins payload.
type pluginsResponse struct {
	Plugins []pluginrt.Status `json:"plugins"`
}

func (s *Server) enabledPlugins() []string {
	if s.settings == nil {
		return nil
	}
	return s.settings.Load().BuiltinPlugins
}

func (s *Server) handleGetPlugins(w http.ResponseWriter, _ *http.Request) {
	enabled := s.enabledPlugins()
	var list []pluginrt.Status
	if s.plugins != nil {
		list = s.plugins(enabled)
	} else {
		// No runtime manager (tests, embedders): catalog + enabled flags only.
		enabledSet := map[string]bool{}
		for _, name := range enabled {
			enabledSet[name] = true
		}
		for _, b := range pluginrt.Builtins() {
			list = append(list, pluginrt.Status{
				Name: b.Name, Kind: b.Kind, Version: b.Version, Description: b.Description,
				Tools: b.Tools, Runtime: b.Runtime, Enabled: enabledSet[b.Name],
				State: pluginrt.StateNotInstalled,
			})
		}
	}
	for i := range list {
		if list[i].Tools == nil {
			list[i].Tools = []string{}
		}
	}
	if list == nil {
		list = []pluginrt.Status{}
	}
	writeJSON(w, http.StatusOK, pluginsResponse{Plugins: list})
}

func (s *Server) handlePutPlugins(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings database is disabled")
		return
	}
	var req struct {
		Enabled []string `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Unknown names are rejected so typos cannot silently hide a plugin.
	known := map[string]bool{}
	for _, b := range pluginrt.Builtins() {
		known[b.Name] = true
	}
	for _, name := range req.Enabled {
		if !known[name] {
			writeError(w, http.StatusBadRequest, "unknown plugin "+name)
			return
		}
	}

	cfg := s.settings.Load()
	if err := settings.Apply(&cfg, settings.KeyBuiltinPlugins, settings.FormatNameList(req.Enabled)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Rebind the agent's tool set right away so the next turn (even within
	// an idle session) sees the change; the between-turn refresh keeps it
	// consistent afterwards.
	if s.agent != nil {
		s.agent.RefreshToolsNow(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request) {
	if s.installPlugin == nil {
		writeError(w, http.StatusServiceUnavailable, "plugin installation is not available in this session")
		return
	}
	name := r.PathValue("name")
	if _, ok := pluginrt.ByName(name); !ok {
		writeError(w, http.StatusNotFound, "unknown plugin "+name)
		return
	}
	// /reinstall deletes the environment first (upgrade/repair); installs
	// run in the background (downloads), progress via GET polling.
	reinstall := r.URL.Path == "/api/plugins/"+name+"/reinstall"
	if err := s.installPlugin(context.WithoutCancel(r.Context()), name, reinstall); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "installing": name})
}
