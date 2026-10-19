// Sandbox backend CRUD (design §4.3): the management plane registers
// candidate sandbox backends, guards the single-enabled invariant, and
// stores each backend's API key encrypted under "sandbox-api-key:<id>".
// The key itself never rides any CRUD response — only key_set does.
package configsvc

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// sandboxTypes is the full contract enum (design §10.3 m-1): a row may
// register any of the seven; the provider registry decides what is
// actually constructible at runtime (verify reports the gap).
var sandboxTypes = []string{
	"contract-http", "e2b", "daytona", "modal", "microsandbox", "cfbridge", "sidecar",
}

func validSandboxType(t string) bool {
	for _, v := range sandboxTypes {
		if v == t {
			return true
		}
	}
	return false
}

// sandboxKeySecret is the settings secret name holding a backend's key.
func sandboxKeySecret(id string) string { return "sandbox-api-key:" + id }

// sandboxPayload is one backend over the wire. KeySet reports whether the
// encrypted key secret exists; the key never leaves the store.
type sandboxPayload struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Endpoint       string `json:"endpoint"`
	TimeoutMs      int64  `json:"timeout_ms"`
	Enabled        bool   `json:"enabled"`
	KeySet         bool   `json:"key_set"`
	LastProbeAt    int64  `json:"last_probe_at"`
	LastProbeOK    bool   `json:"last_probe_ok"`
	LastProbeError string `json:"last_probe_error,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

// sandboxLog is the shared structured-log shape for sandbox writes
// (design §4.6.5). The console gate carries no per-request identity, so
// every write is attributed to "console".
func (s *Service) sandboxLog(action, id, typ string) *zerolog.Event {
	return s.log.Info().Str("action", action).Str("backend_id", id).Str("type", typ).Str("operator", "console")
}

// listSandboxRows reads the full inventory.
func (s *Service) listSandboxRows() ([]sandboxPayload, error) {
	rows, err := s.db.Query(`SELECT id, name, type, endpoint, timeout_ms, enabled,
		last_probe_at, last_probe_ok, last_probe_error, updated_at FROM sandbox_backends ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []sandboxPayload{}
	for rows.Next() {
		var p sandboxPayload
		var enabled int64
		var probeAt, probeOK sql.NullInt64
		var probeErr sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.Endpoint, &p.TimeoutMs, &enabled,
			&probeAt, &probeOK, &probeErr, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		p.LastProbeOK = probeOK.Valid && probeOK.Int64 != 0
		p.LastProbeAt = probeAt.Int64
		p.LastProbeError = probeErr.String
		p.KeySet = s.settings.HasSecret(sandboxKeySecret(p.ID))
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) handleListSandboxes(w http.ResponseWriter, _ *http.Request) {
	list, err := s.listSandboxRows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sandbox inventory unavailable")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// validateSandboxFields checks the shared invariants; empty strings mean
// "absent, keep current".
func validateSandboxFields(name, typ, endpoint string, timeoutMs int64) string {
	if name != "" && strings.TrimSpace(name) == "" {
		return "name is required"
	}
	if typ != "" && !validSandboxType(typ) {
		return "type must be one of: " + strings.Join(sandboxTypes, ", ")
	}
	if endpoint != "" {
		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return "endpoint must be an absolute URL (scheme://host:port)"
		}
	}
	if timeoutMs != 0 && timeoutMs < 1000 {
		return "timeout_ms must be at least 1000"
	}
	return ""
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key")
}

func (s *Service) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Endpoint  string `json:"endpoint"`
		TimeoutMs int64  `json:"timeout_ms"`
		APIKey    string `json:"api_key"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Type = strings.TrimSpace(req.Type)
	if req.Type == "" {
		req.Type = "contract-http"
	}
	if req.TimeoutMs == 0 {
		req.TimeoutMs = 120000
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if msg := validateSandboxFields(req.Name, req.Type, req.Endpoint, req.TimeoutMs); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	now := wocadb.Now()
	var rnd [4]byte
	if _, err := crand.Read(rnd[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	id := fmt.Sprintf("sb-%x-%x", now, rnd)
	err := s.db.WithMigrateLock(func(q wocadb.Querier) error {
		if req.Enabled {
			if _, err := q.Exec(`UPDATE sandbox_backends SET enabled = 0`); err != nil {
				return err
			}
		}
		var enabled int64
		if req.Enabled {
			enabled = 1
		}
		_, err := q.Exec(`INSERT INTO sandbox_backends
			(id, name, type, endpoint, timeout_ms, enabled, created_at, updated_at)
			VALUES (`+phI(s.db, 0)+`, `+phI(s.db, 1)+`, `+phI(s.db, 2)+`, `+phI(s.db, 3)+`, `+phI(s.db, 4)+`, `+phI(s.db, 5)+`, `+phI(s.db, 6)+`, `+phI(s.db, 7)+`)`,
			id, req.Name, req.Type, req.Endpoint, req.TimeoutMs, enabled, now, now)
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, "name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if err := s.settings.SetSecret(sandboxKeySecret(id), req.APIKey); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sandboxLog("sandbox.create", id, req.Type).Str("name", req.Name).Msg("sandbox backend registered via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sandboxPayload{
		ID: id, Name: req.Name, Type: req.Type, Endpoint: req.Endpoint,
		TimeoutMs: req.TimeoutMs, Enabled: req.Enabled,
		KeySet: s.settings.HasSecret(sandboxKeySecret(id)), UpdatedAt: now,
	})
}

// getSandbox reads one row by id; ok=false means no such backend.
func (s *Service) getSandbox(id string) (sandboxPayload, bool, error) {
	var p sandboxPayload
	var enabled int64
	var probeAt, probeOK sql.NullInt64
	var probeErr sql.NullString
	err := s.db.QueryRow(`SELECT id, name, type, endpoint, timeout_ms, enabled,
		last_probe_at, last_probe_ok, last_probe_error, updated_at FROM sandbox_backends WHERE id = `+phI(s.db, 0), id).
		Scan(&p.ID, &p.Name, &p.Type, &p.Endpoint, &p.TimeoutMs, &enabled,
			&probeAt, &probeOK, &probeErr, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.Enabled = enabled != 0
	p.LastProbeOK = probeOK.Valid && probeOK.Int64 != 0
	p.LastProbeAt = probeAt.Int64
	p.LastProbeError = probeErr.String
	p.KeySet = s.settings.HasSecret(sandboxKeySecret(p.ID))
	return p, true, nil
}

func (s *Service) handleUpdateSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, ok, err := s.getSandbox(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown sandbox backend")
		return
	}
	var req struct {
		Name      *string `json:"name"`
		Type      *string `json:"type"`
		Endpoint  *string `json:"endpoint"`
		TimeoutMs *int64  `json:"timeout_ms"`
		Enabled   *bool   `json:"enabled"`
		APIKey    *string `json:"api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Resolve the effective values for validation, then update only the
	// present fields (partial update).
	name, typ, endpoint, timeout := cur.Name, cur.Type, cur.Endpoint, cur.TimeoutMs
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Type != nil {
		typ = strings.TrimSpace(*req.Type)
	}
	if req.Endpoint != nil {
		endpoint = strings.TrimSpace(*req.Endpoint)
	}
	if req.TimeoutMs != nil {
		timeout = *req.TimeoutMs
	}
	if msg := validateSandboxFields(name, typ, endpoint, timeout); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	now := wocadb.Now()
	err = s.db.WithMigrateLock(func(q wocadb.Querier) error {
		if req.Enabled != nil && *req.Enabled {
			// v1 allows exactly one enabled backend: demote every other
			// row in the same write.
			if _, err := q.Exec(`UPDATE sandbox_backends SET enabled = 0 WHERE id != `+phI(s.db, 0), id); err != nil {
				return err
			}
		}
		sets := []string{"updated_at = " + phI(s.db, 0)}
		args := []any{now}
		add := func(col string, v any, ph int) {
			sets = append(sets, col+" = "+phI(s.db, ph))
			args = append(args, v)
		}
		if req.Name != nil {
			add("name", name, len(args))
		}
		if req.Type != nil {
			add("type", typ, len(args))
		}
		if req.Endpoint != nil {
			add("endpoint", endpoint, len(args))
		}
		if req.TimeoutMs != nil {
			add("timeout_ms", timeout, len(args))
		}
		if req.Enabled != nil {
			var v int64
			if *req.Enabled {
				v = 1
			}
			add("enabled", v, len(args))
		}
		args = append(args, id)
		_, err := q.Exec(`UPDATE sandbox_backends SET `+strings.Join(sets, ", ")+
			` WHERE id = `+phI(s.db, len(args)-1), args...)
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, "name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	// A non-empty key overwrites the stored secret; empty keeps it.
	if req.APIKey != nil && *req.APIKey != "" {
		if err := s.settings.SetSecret(sandboxKeySecret(id), *req.APIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.sandboxLog("sandbox.update", id, typ).Msg("sandbox backend updated via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	after, _, err := s.getSandbox(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, after)
}

func (s *Service) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, ok, err := s.getSandbox(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown sandbox backend")
		return
	}
	if cur.Enabled {
		writeError(w, http.StatusConflict, "disable the backend before deleting it")
		return
	}
	res, err := s.db.Exec(`DELETE FROM sandbox_backends WHERE id = `+phI(s.db, 0), id)
	if err != nil || nRows(res) != 1 {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	// The key must not outlive its row (design §10.2 M-5): an empty
	// SetSecret removes it.
	if err := s.settings.SetSecret(sandboxKeySecret(id), ""); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sandboxLog("sandbox.delete", id, cur.Type).Msg("sandbox backend deleted via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
