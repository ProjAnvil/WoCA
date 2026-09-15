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
	"cmp"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

func phI(db *wocadb.DB, i int) string { return db.Dialect.Placeholder(i) }

func nRows(res sql.Result) int64 {
	n, _ := res.RowsAffected()
	return n
}

// requireScope wraps h with the route's scope check (gateway.RequireScope).
func (s *Service) requireScope(scope string, h http.HandlerFunc) http.Handler {
	return gateway.RequireScope(authz.NewLocal(), scope)(h)
}

func (s *Service) handleGet(w http.ResponseWriter, _ *http.Request) {
	cfg := s.settings.Load()
	writeJSON(w, http.StatusOK, payload{
		Provider:                cfg.Provider,
		Model:                   cfg.Model,
		MaxTokens:               cfg.MaxTokens,
		Temperature:             formatFloat(cfg.Temperature),
		AnthropicBaseURL:        cfg.AnthropicBaseURL,
		OpenAIBaseURL:           cfg.OpenAIBaseURL,
		GLMBaseURL:              cfg.GLMBaseURL,
		DeepSeekBaseURL:         cfg.DeepSeekBaseURL,
		CheckpointBackend:       cfg.CheckpointBackend,
		MemoryBackend:           cfg.MemoryBackend,
		CheckpointDir:           cfg.CheckpointDir,
		Database:                s.settings.Path(),
		DebugLog:                cfg.DebugLog,
		MemoryEngine:            cfg.MemoryEngine,
		LadyMDBPath:             cfg.LadyMDBPath,
		LadyMEmbedding:          cfg.LadyMEmbedding,
		LadyMEmbeddingModel:     cfg.LadyMEmbeddingModel,
		LadyMEmbeddingBaseURL:   cfg.LadyMEmbeddingBaseURL,
		LadyMLLM:                cfg.LadyMLLM,
		LadyMAutoRetain:         cfg.LadyMAutoRetain,
		LadyMTopK:               cfg.LadyMTopK,
		LadyMSystem2:            cfg.LadyMSystem2,
		LadyMSystem2Interval:    cfg.LadyMSystem2Interval,
		LadyMEndpoint:           cfg.LadyMEndpoint,
		LadyMAPIUsername:        cfg.LadyMAPIUsername,
		PostgresURLSet:          s.settings.HasSecret(settings.SecretPostgresURL),
		LadyMAPIPasswordSet:     s.settings.HasSecret(settings.SecretLadyMAPIPassword),
		CheckpointPGHost:        cfg.CheckpointPGHost,
		CheckpointPGPort:        orDefault(cfg.CheckpointPGPort, "5432"),
		CheckpointPGDB:          orDefault(cfg.CheckpointPGDB, "woca"),
		CheckpointPGUser:        cfg.CheckpointPGUser,
		CheckpointPGPasswordSet: s.settings.HasSecret(settings.SecretCheckpointPGPassword),
		MemoryPGHost:            cfg.MemoryPGHost,
		MemoryPGPort:            orDefault(cfg.MemoryPGPort, "5432"),
		MemoryPGDB:              orDefault(cfg.MemoryPGDB, "woca"),
		MemoryPGUser:            cfg.MemoryPGUser,
		MemoryPGPasswordSet:     s.settings.HasSecret(settings.SecretMemoryPGPassword),
		MsgPlaneMode:            orDefault(cfg.MsgPlaneMode, "pg"),
		MsgPlaneNATSURL:         cfg.MsgPlaneNATSURL,
		MsgPlaneSubject:         orDefault(cfg.MsgPlaneSubject, "runs.dispatch"),
		// The config plane manages the keys for every deployment form that
		// reads this database; availability reporting belongs to the planes
		// that know their own topology (the business server reports its own
		// dispatch state).
		MsgPlaneAvailable: true,
	})
}

// handleUpdate applies an update: settings keys through Apply (enum and
// range validation included), secrets through the encrypted store. The
// response reports which effect classes the change triggers so operators
// see what a restart vs a live rebuild means; the live rebuild itself
// happens in the business roles via the settings-generation watcher.
func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var u update
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.settings.Load()
	for _, p := range u.patches() {
		// The enterprise config plane only offers postgres-grade
		// persistence (product ruling: no embedded sqlite/file/memory
		// stores behind the enterprise console) — reject anything else
		// before it reaches the settings tables.
		if (p.key == settings.KeyCheckpoint || p.key == settings.KeyMemory) && *p.value != "postgres" {
			writeError(w, http.StatusBadRequest, "the enterprise config plane only supports the postgres backend for checkpoint and memory (got "+*p.value+")")
			return
		}
		if err := settings.Apply(&cfg, p.key, *p.value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range u.secretPatches() {
		if p.value == nil {
			continue
		}
		if err := s.settings.SetSecret(p.key, *p.value); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.log.Info().Strs("keys", updateKeys(&u)).Msg("settings updated via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// exportFile is the declarative config document (design §16.4): the plain
// settings values plus, optionally, decrypted secrets for Git-safe full
// restores.
type exportFile struct {
	Values  map[string]string `json:"values"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

func (s *Service) handleExport(w http.ResponseWriter, r *http.Request) {
	cfg := s.settings.Load()
	values := map[string]string{}
	for _, k := range settings.KeyNames() {
		if v := settings.KeyValue(cfg, k); v != "" {
			values[k] = v
		}
	}
	out := exportFile{Values: values}
	if r.URL.Query().Get("include_secrets") == "true" {
		secrets := map[string]string{}
		for _, name := range []string{
			settings.SecretAnthropicAPIKey, settings.SecretOpenAIAPIKey,
			settings.SecretGLMAPIKey, settings.SecretDeepSeekAPIKey,
			settings.SecretPostgresURL, settings.SecretLadyMEmbeddingAPIKey,
			settings.SecretCheckpointPGPassword, settings.SecretMemoryPGPassword,
		} {
			if v := s.settings.GetSecret(name); v != "" {
				secrets[name] = v
			}
		}
		if len(secrets) > 0 {
			out.Secrets = secrets
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleImport idempotently applies an export document: every values entry
// through Apply (so invalid documents fail atomically before any write),
// then secrets. An empty document is a no-op.
func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	var doc exportFile
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.settings.Load()
	for k, v := range doc.Values {
		if _, isSecret := settings.SecretFor(k); isSecret {
			writeError(w, http.StatusBadRequest, k+" is a secret; put it in the secrets section")
			return
		}
		if err := settings.Apply(&cfg, k, v); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.settings.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for name, v := range doc.Secrets {
		if err := s.settings.SetSecret(name, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.log.Info().Int("values", len(doc.Values)).Int("secrets", len(doc.Secrets)).Msg("settings imported via config plane")
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "values": len(doc.Values), "secrets": len(doc.Secrets)})
}

// ackStaleAfter bounds how fresh an ack must be to count as live
// (design §16.7: the ack row doubles as the heartbeat).
const ackStaleAfter = int64(15_000)

// handleStatus reports the distribution inventory (design §16.7): the
// global settings + tenant generations and every service's self-reported
// ack, so operators see who received what — and who fell behind.
func (s *Service) handleStatus(w http.ResponseWriter, _ *http.Request) {
	gen := s.settings.Generation()
	tenantGen := s.tenants.Generation()
	resp := map[string]any{
		"generation":        gen,
		"tenant_generation": tenantGen,
		"services":          []map[string]any{},
	}
	if s.db != nil {
		// Janitor pass: acks from dead container generations (hostnames
		// churn on every recreate) age out of the inventory after ten
		// minutes — well past the 15s liveness window, while a restarting
		// role re-arms its ack within seconds of boot.
		now := wocadb.Now()
		if err := s.db.PruneConfigAcks(now - 600_000); err != nil {
			s.log.Warn().Err(err).Msg("config ack prune failed")
		}
		acks, err := s.db.ConfigAcks()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		slices.SortFunc(acks, func(a, b wocadb.ConfigAck) int { return cmp.Compare(a.ServiceID, b.ServiceID) })
		services := make([]map[string]any, 0, len(acks))
		for _, a := range acks {
			services = append(services, map[string]any{
				"service_id":     a.ServiceID,
				"role":           a.Role,
				"generation":     a.Generation,
				"addr":           a.Addr,
				"updated_at":     a.UpdatedAt,
				"live":           now-a.UpdatedAt < ackStaleAfter,
				"config_lagging": a.Generation != gen,
			})
		}
		resp["services"] = services
	}
	writeJSON(w, http.StatusOK, resp)
}
