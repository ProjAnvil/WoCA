// Package opsconsole is the tenant ops console (product design v1.8
// §6.7): the private core's single management surface, hosted by the
// service plane (woca-server's internal listener) and reachable only on
// the private network — the edge access point never forwards it. It owns
// the tenant credentials (woca_ keys over the platform tenant tables,
// full key shown exactly once, offline distribution, core-side
// last_used), the sessions the core issued (view/revoke — §2.1), the
// bridge enrollment inventory (one-time tokens, worker-led approvals,
// registration list, revocation), the governance audit trail, and the
// gateway policy (edge SPKI pin).
//
// Neither the bridge nor the config console carries any of this: bridges
// hold zero user credentials (v1.8), and the config plane's console
// stays focused on woca's own settings domains.
package opsconsole

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/projanvil/woca/internal/platform/httpx"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/consoleauth"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

//go:embed all:spa
var spaFS embed.FS

// Service wires the ops console over the shared database: the gateway
// registry (entrygw), the credential/session authority (authcore over
// the platform tenant tables), and the settings store all share it.
type Service struct {
	gw      *entrygw.Store
	auth    *authcore.Service
	keys    *tenant.Store
	db      *wocadb.DB
	log     zerolog.Logger
	console *consoleauth.Gate
}

// New builds the console service (and seeds the gateway posture from the
// startup environment — the certificate-less bridge switch is a
// deployment decision made at process start, never a page toggle, R6).
func New(st *settings.Store, gw *entrygw.Store, auth *authcore.Service, keys *tenant.Store, log zerolog.Logger) *Service {
	s := &Service{gw: gw, auth: auth, keys: keys, db: st.DB(), log: log, console: consoleauth.New()}
	s.seedGatewayPosture()
	return s
}

// seedGatewayPosture installs the certificate-less bridge posture from
// the startup environment (WOCA_INSECURE_BRIDGES=1). Seeding every boot
// makes the env authoritative and overrides any stale row.
func (s *Service) seedGatewayPosture() {
	v := "false"
	if os.Getenv("WOCA_INSECURE_BRIDGES") == "1" {
		v = "true"
		s.log.Warn().Msg("opsconsole: certificate-less bridge posture ENABLED via WOCA_INSECURE_BRIDGES=1 (development only — never in production, R6)")
	}
	if err := s.gw.SetSetting(context.Background(), entrygw.SettingKeyAllowInsecure, v); err != nil {
		s.log.Warn().Err(err).Msg("opsconsole: gateway posture seed failed")
	}
}

// Handler returns the console surface: the login exchange and the SPA's
// non-API fallback mount at the top level; every /api route sits behind
// the console gate and its per-route scope check.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-ops-console"})
	})
	// Tenants & API keys (the private cloud's own credentials, v1.8:
	// woca_ keys over the platform tenant tables — the sole credential
	// format in the product; the concept set is tenant/user/api_key):
	// issue / list / revoke. Issued keys are shown exactly once;
	// revocation kills the key AND its core-issued sessions on the spot.
	mux.Handle("GET /api/tenant/tenants", s.requireScope(authz.ScopeSettingsRead, s.handleListTenants))
	mux.Handle("POST /api/tenant/tenants", s.requireScope(authz.ScopeSettingsWrite, s.handleCreateTenant))
	mux.Handle("GET /api/tenant/stats", s.requireScope(authz.ScopeSettingsRead, s.handleTenantStats))
	mux.Handle("POST /api/tenant/users", s.requireScope(authz.ScopeSettingsWrite, s.handleUserIssue))
	mux.Handle("GET /api/tenant/users", s.requireScope(authz.ScopeSettingsRead, s.handleUserList))
	mux.Handle("DELETE /api/tenant/users/{prefix}", s.requireScope(authz.ScopeSettingsWrite, s.handleUserRevoke))
	// Core-issued sessions (§2.1): the live inventory with one-click
	// revocation per session.
	mux.Handle("GET /api/tenant/sessions", s.requireScope(authz.ScopeSettingsRead, s.handleSessionList))
	mux.Handle("DELETE /api/tenant/sessions/{hash}", s.requireScope(authz.ScopeSettingsWrite, s.handleSessionRevoke))
	// Bridge enrollment: one-time token issuance, worker-led approvals,
	// the registration inventory with one-click revocation.
	mux.Handle("POST /api/gw/enrollments", s.requireScope(authz.ScopeSettingsWrite, s.handleIssueEnrollment))
	mux.Handle("GET /api/gw/pending", s.requireScope(authz.ScopeSettingsRead, s.handlePendingEnrollments))
	mux.Handle("POST /api/gw/pending/{fingerprint}/approve", s.requireScope(authz.ScopeSettingsWrite, s.handleApproveEnrollment))
	mux.Handle("GET /api/gw/bridges", s.requireScope(authz.ScopeSettingsRead, s.handleListBridges))
	mux.Handle("DELETE /api/gw/bridges/{id}", s.requireScope(authz.ScopeSettingsWrite, s.handleRevokeBridge))
	// Governance audit trail (R5): key/bridge issuance and revocation,
	// enrollments, approvals.
	mux.Handle("GET /api/gw/audit", s.requireScope(authz.ScopeSettingsRead, s.handleGwAudit))
	mux.Handle("GET /api/gw/audit/events", s.requireScope(authz.ScopeSettingsRead, s.handleGwAuditEvents))
	// Gateway policy: the edge SPKI pin embedded in enrollment tokens.
	mux.Handle("GET /api/gw/settings", s.requireScope(authz.ScopeSettingsRead, s.handleGetGatewaySettings))
	mux.Handle("PUT /api/gw/settings", s.requireScope(authz.ScopeSettingsWrite, s.handlePutGatewaySettings))

	spaRoot, err := fs.Sub(spaFS, "spa")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", spaWithFallback(spaRoot))

	top := http.NewServeMux()
	// The login route must be reachable before a session exists, so it
	// cannot live behind the gate it bootstraps.
	top.HandleFunc("POST /api/console/login", s.console.Login)
	top.HandleFunc("POST /api/console/logout", s.console.Logout)
	top.Handle("/", s.console.Wrap(mux))
	return top
}

// platformTenant is the private cloud's own trust circle: every console
// credential lives as a user of this tenant (§2.1: the concept set is
// tenant/user/api_key — one credential format, woca_).
const platformTenant = "local"

// userRow is the console's user list shape (the wire contract).
type userRow struct {
	Prefix     string `json:"prefix"`
	Remark     string `json:"remark"`
	Role       string `json:"role"`
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	IssuedAt   int64  `json:"issued_at"`
	LastUsed   int64  `json:"last_used,omitempty"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
}

// tenantRow is the console's tenant wire shape.
type tenantRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// handleListTenants serves the tenant (business-unit) registry.
func (s *Service) handleListTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := s.keys.ListTenants(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	out := make([]tenantRow, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, tenantRow{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateTenant creates one business unit.
func (s *Service) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if len(name) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name too long (max 64 characters)"})
		return
	}
	t, err := s.keys.CreateTenant(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.gw.AuditEvent(r.Context(), "tenant.created", t.ID, "name="+name)
	writeJSON(w, http.StatusOK, tenantRow{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt})
}

// tenantStats is one business unit's usage snapshot (the console's
// per-tenant display: credential counts and observed activity).
type tenantStats struct {
	TenantID    string `json:"tenant_id"`
	TenantName  string `json:"tenant_name"`
	KeysActive  int64  `json:"keys_active"`
	KeysRevoked int64  `json:"keys_revoked"`
	Sessions    int64  `json:"sessions"`
	LastUsed    int64  `json:"last_used,omitempty"`
}

// handleTenantStats aggregates per-tenant counts (keys, live sessions,
// last core-observed activity).
func (s *Service) handleTenantStats(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.db.Query(`
		SELECT t.id, t.name,
		       COALESCE(SUM(CASE WHEN k.revoked_at IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN k.revoked_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(MAX(k.last_used), 0)
		FROM tenants t LEFT JOIN api_keys k ON k.tenant_id = t.id
		GROUP BY t.id, t.name ORDER BY t.name`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	defer rows.Close()
	byID := map[string]*tenantStats{}
	order := []string{}
	for rows.Next() {
		var st tenantStats
		if err := rows.Scan(&st.TenantID, &st.TenantName, &st.KeysActive, &st.KeysRevoked, &st.LastUsed); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
			return
		}
		byID[st.TenantID] = &st
		order = append(order, st.TenantID)
	}
	// Live (unrevoked, unexpired) sessions per tenant.
	srows, err := s.db.Query(`
		SELECT c.tenant_id, COUNT(*) FROM core_sessions c
		WHERE c.revoked_at IS NULL AND c.expires_at > `+phI(s.db, 0)+`
		GROUP BY c.tenant_id`, wocadb.Now())
	if err == nil {
		defer srows.Close()
		for srows.Next() {
			var tid string
			var n int64
			if srows.Scan(&tid, &n) == nil {
				if st, ok := byID[tid]; ok {
					st.Sessions = n
				}
			}
		}
	}
	out := make([]tenantStats, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUserIssue mints one tenant credential: find-or-create the user
// by remark inside the platform tenant, then issue a woca_ key (the
// full key is shown exactly once, offline distribution).
func (s *Service) handleUserIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Remark   string `json:"remark"`
		Role     string `json:"role"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	remark := strings.TrimSpace(req.Remark)
	if remark == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remark (username) is required"})
		return
	}
	if len(remark) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remark too long (max 64 characters)"})
		return
	}
	tenantID := strings.TrimSpace(req.TenantID)
	if tenantID == "" {
		tenantID = platformTenant
	}
	if _, err := s.keys.TenantByID(r.Context(), tenantID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown tenant — create it first"})
		return
	}
	u, err := s.keys.UserByName(r.Context(), tenantID, remark)
	if errors.Is(err, tenant.ErrNotFound) {
		u, err = s.keys.CreateUser(r.Context(), tenantID, remark, req.Role)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	k, full, err := s.keys.IssueKey(r.Context(), tenantID, u.ID, remark, req.Role)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.gw.AuditEvent(r.Context(), "user.issued", k.Prefix, "tenant="+tenantID+" remark="+remark+" role="+k.Role)
	writeJSON(w, http.StatusOK, map[string]any{
		"key":  full,
		"note": "shown exactly once — deliver to the user out of band",
		"user": userRow{Prefix: k.Prefix, Remark: k.Name, Role: k.Role, IssuedAt: k.CreatedAt, TenantID: k.TenantID, TenantName: k.TenantName},
	})
}

// pageParams parses the shared pager query (?q=&limit=&offset=).
func pageParams(r *http.Request) (q string, limit, offset int) {
	q = strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset = 50, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil {
		offset = v
	}
	return q, limit, offset
}

func phI(db *wocadb.DB, i int) string { return db.Dialect.Placeholder(i) }

// orderParam reads ?order= (whitelisted downstream; never interpolated
// raw into SQL).
func orderParam(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("order"))
}

// handleUserList serves one page of the credential registry (?q=&limit=&offset=
// — the database does the filtering and paging). last_used is
// authoritative here (logins and validations happen at the core).
func (s *Service) handleUserList(w http.ResponseWriter, r *http.Request) {
	q, limit, offset := pageParams(r)
	// The console's order names map onto the store whitelist.
	order := map[string]string{
		"issued": "created_at", "issued_desc": "created_at_desc",
		"last_used": "last_used", "last_used_desc": "last_used_desc",
	}[orderParam(r)]
	// The console lists across every business unit; an explicit tenant
	// filter narrows to one.
	tenantFilter := strings.TrimSpace(r.URL.Query().Get("tenant"))
	listFn := s.keys.ListKeysForAll
	if tenantFilter != "" {
		listFn = func(ctx context.Context, qq, o string, l, off int) ([]tenant.APIKey, int, error) {
			return s.keys.ListKeys(ctx, tenantFilter, qq, o, l, off)
		}
	}
	keys, total, err := listFn(r.Context(), q, order, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	out := make([]userRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, userRow{
			Prefix: k.Prefix, Remark: k.Name, Role: k.Role,
			TenantID: k.TenantID, TenantName: k.TenantName,
			IssuedAt: k.CreatedAt, LastUsed: k.LastUsed, RevokedAt: k.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": total})
}

// handleUserRevoke kills a credential and every session it issued —
// immediate, final (§2.1).
func (s *Service) handleUserRevoke(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	if prefix == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prefix required"})
		return
	}
	if _, ok, err := s.keys.KeyByPrefix(r.Context(), prefix); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	} else if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown or already-revoked key prefix"})
		return
	}
	if err := s.keys.RevokeKey(r.Context(), prefix); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	if err := s.auth.RevokeKeySessions(r.Context(), prefix); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.gw.AuditEvent(r.Context(), "user.revoked", prefix, "sessions killed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSessionList serves one page of the login-state inventory
// (?q=&limit=&offset= — database-side filtering and paging).
func (s *Service) handleSessionList(w http.ResponseWriter, r *http.Request) {
	q, limit, offset := pageParams(r)
	order := map[string]string{
		"issued": "created_at", "issued_desc": "created_at_desc",
		"last_used": "last_seen", "last_used_desc": "last_seen_desc",
		"expires": "expires_at", "expires_desc": "expires_at_desc",
	}[orderParam(r)]
	sessions, total, err := s.auth.ListSessions(r.Context(), q, order, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": sessions, "total": total})
}

// handleSessionRevoke kills one session (the next validation fails).
func (s *Service) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if len(hash) != 64 || !isHex64(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hash must be a 64-char hex SHA-256 digest"})
		return
	}
	if err := s.auth.RevokeSession(hash); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.gw.AuditEvent(r.Context(), "session.revoked", hash[:12], "console") // len validated above
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListBridges serves the registration inventory.
func (s *Service) handleListBridges(w http.ResponseWriter, r *http.Request) {
	bridges, err := s.gw.ListBridges(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, bridges)
}

// handleRevokeBridge deregisters a bridge: the record is marked revoked
// and every outstanding access token dies immediately — the edge's next
// call-home resolve fails, which kicks the long-poll and tunnel off (the
// §2.3 revocation semantics).
func (s *Service) handleRevokeBridge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bridge id required"})
		return
	}
	if err := s.gw.RevokeBridge(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.gw.AuditEvent(r.Context(), "bridge.revoked", id, "console")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGwAudit serves one page of the governance trail with filters:
// ?bridge=&event=&from=&to= (millisecond bounds, zero = open) plus the
// shared ?limit=&offset= pager. The event vocabulary for the filter
// dropdown comes from /api/gw/audit/events.
func (s *Service) handleGwAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, limit, offset := pageParams(r)
	f := entrygw.AuditFilter{
		BridgeID: strings.TrimSpace(q.Get("bridge")),
		Event:    strings.TrimSpace(q.Get("event")),
	}
	if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
		f.From = v
	}
	if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
		f.To = v
	}
	rows, total, err := s.gw.AuditEvents(r.Context(), f, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	if rows == nil {
		rows = []entrygw.GwAuditRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total})
}

// handleGwAuditEvents serves the distinct event names (the filter
// dropdown's vocabulary).
func (s *Service) handleGwAuditEvents(w http.ResponseWriter, _ *http.Request) {
	names, err := s.gw.AuditEventNames()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, names)
}

// handleIssueEnrollment mints a one-time enrollment token.
func (s *Service) handleIssueEnrollment(w http.ResponseWriter, r *http.Request) {
	token, err := s.gw.IssueEnrollment(r.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	s.gw.AuditEvent(r.Context(), "enrollment.issued", "", fmt.Sprintf("ttl=%s r.RemoteAddr=%s", entrygw.EnrollmentTTL, r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_token": token,
		"expires_in_s":     int(entrygw.EnrollmentTTL.Seconds()),
		"note":             "single use; paste it on the bridge's cloud-access page (Settings → Cloud access)",
	})
}

// handlePendingEnrollments lists worker-led enrollment requests.
func (s *Service) handlePendingEnrollments(w http.ResponseWriter, r *http.Request) {
	pending, err := s.gw.PendingRequests(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, pending)
}

// handleApproveEnrollment approves a worker-led enrollment request.
func (s *Service) handleApproveEnrollment(w http.ResponseWriter, r *http.Request) {
	if _, err := s.gw.ApproveEnrollment(r.Context(), r.PathValue("fingerprint")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown fingerprint"})
		return
	}
	s.gw.AuditEvent(r.Context(), "enrollment.approved", "", "fingerprint="+r.PathValue("fingerprint"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGetGatewaySettings serves the gateway policy for the console.
// The certificate-less posture is absent on purpose: it is a startup
// environment decision, not a page setting.
func (s *Service) handleGetGatewaySettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.gw.Settings(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"edge_spki_pin": settings[entrygw.SettingKeyEdgeSPKIPin],
	})
}

// handlePutGatewaySettings updates the gateway policy (the edge SPKI pin
// embedded in enrollment tokens — the k3s-mode anti-poisoning pin).
func (s *Service) handlePutGatewaySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EdgeSPKIPin *string `json:"edge_spki_pin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.EdgeSPKIPin == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	pin := strings.ToLower(strings.TrimSpace(*req.EdgeSPKIPin))
	if pin != "" && !isHex64(pin) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "edge_spki_pin must be a 64-char hex SHA-256 digest"})
		return
	}
	if err := s.gw.SetSetting(r.Context(), entrygw.SettingKeyEdgeSPKIPin, pin); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	if pin != "" {
		s.gw.AuditEvent(r.Context(), "edge.pin_set", "", "spki="+pin[:12])
	} else {
		s.gw.AuditEvent(r.Context(), "edge.pin_cleared", "", "console")
	}
	writeJSON(w, http.StatusOK, map[string]any{"edge_spki_pin": pin})
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// requireScope wraps h with the route's scope check (gateway.RequireScope
// over the console's administrator context).
func (s *Service) requireScope(scope string, h http.HandlerFunc) http.Handler {
	return gateway.RequireScope(authz.NewLocal(), scope)(h)
}

// spaWithFallback serves SPA assets and falls back to index.html for
// client-side routes (unknown non-API paths render the app).
func spaWithFallback(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	ix, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if _, err := fs.Stat(root, name); err != nil {
				// Client-side route: render the app shell.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(ix)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}
