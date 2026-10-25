// admin.go is the bridge's read-only owner surface over HTTP (product
// design §5.1 SPA admin page, v1.7): the live-session inventory and the local
// audit trail. There is no key management here — organization
// credentials are issued by the tenant ops console on the private core,
// never on the bridge.
package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

// Audit is the bridge-local audit log (R5): who (key remark / user id),
// when, what. The full key shows exactly once at issue time; the trail
// never stores secrets.
type Audit struct {
	db *sql.DB
	ph func(int) string
}

// OpenAudit opens (and creates) the audit table on the shared database.
func OpenAudit(wdb *wocadb.DB) (*Audit, error) {
	if _, err := wdb.Exec(`CREATE TABLE IF NOT EXISTS bridge_audit (
		at INTEGER NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return nil, fmt.Errorf("bridge: audit table: %w", err)
	}
	return &Audit{db: wdb.DB, ph: wdb.Dialect.Placeholder}, nil
}

// Record appends one audit event (best-effort; failures never fail the
// audited operation).
func (a *Audit) Record(ctx context.Context, actor, action, detail string) {
	if a == nil {
		return
	}
	_, _ = a.db.ExecContext(ctx,
		`INSERT INTO bridge_audit (at, actor, action, detail) VALUES (`+a.ph(0)+`, `+a.ph(1)+`, `+a.ph(2)+`, `+a.ph(3)+`)`,
		time.Now().UnixMilli(), actor, action, detail)
}

// AuditEvent is one audit row.
type AuditEvent struct {
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// List returns the newest events.
func (a *Audit) List(ctx context.Context, limit int) ([]AuditEvent, error) {
	if a == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := a.db.QueryContext(ctx,
		`SELECT at, actor, action, detail FROM bridge_audit ORDER BY at DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// adminRoutes registers the read-only owner surface. /api/auth/me is
// self-identity (any session); the session inventory and audit trail are
// governance data — settings:write, the admin authority, because members
// hold settings:read for the interactive surfaces (see authz.ScopesForRole).
func (a *Auth) RegisterAdminRoutes(mux *http.ServeMux) {
	admin := func(scope string, h http.HandlerFunc) http.Handler {
		return gateway.RequireScope(authz.NewLocal(), scope)(h)
	}
	mux.Handle("GET /api/auth/me", admin(authz.ScopeSessionsRead, a.handleMe))
	mux.Handle("GET /api/sessions/admin", admin(authz.ScopeSettingsWrite, a.handleListSessions))
	mux.Handle("GET /api/audit", admin(authz.ScopeSettingsWrite, a.handleAudit))
}

// AuditEvent records a local audit event with the actor's remark
// resolved when known (the identity came from the core's assertion or
// introspection result; the trail names people). Exported for the serve
// wiring (server.Config.Audit → bridge trail).
func (a *Auth) AuditEvent(ctx context.Context, actor, action, detail string) {
	if remark := a.sessions.remarkFor(actor); remark != "" {
		actor = remark
	}
	a.auditLog.Record(ctx, actor, action, detail)
}

// handleMe reports the caller's identity — the login session's remark and
// role for the SPA's admin gating.
func (a *Auth) handleMe(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenancy.FromContext(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	remark := tc.UserID
	if r := a.sessions.remarkFor(tc.UserID); r != "" {
		remark = r
	}
	role := authz.RoleForScopes(tc.Scopes)
	writeJSONAuth(w, http.StatusOK, map[string]any{
		"user_id": tc.UserID, "remark": remark, "role": role,
	})
}

// SessionInfo is one live login session (the "logged-in devices" inventory).
// KeyPrefix carries the user id (the mirror keys on token hashes; the
// console's per-credential view lives at the core).
type SessionInfo struct {
	KeyPrefix string `json:"key_prefix"`
	TenantID  string `json:"tenant_id,omitempty"`
	Remark    string `json:"remark"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	ExpiresAt int64  `json:"expires_at"`
}

func (a *Auth) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	out := make([]SessionInfo, 0)
	for _, m := range a.sessions.list() {
		out = append(out, SessionInfo{
			KeyPrefix: m.UserID, TenantID: m.TenantID, Remark: m.Remark, Role: m.Role,
			CreatedAt: m.CreatedAt, LastSeen: m.LastSeen, ExpiresAt: m.ExpiresAt,
		})
	}
	writeJSONAuth(w, http.StatusOK, out)
}

func (a *Auth) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := jsonInt(v); err == nil {
			limit = n
		}
	}
	events, err := a.auditLog.List(r.Context(), limit)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSONAuth(w, http.StatusOK, events)
}

func jsonInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}
