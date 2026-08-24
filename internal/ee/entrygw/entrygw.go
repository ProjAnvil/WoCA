// Package entrygw implements the private-core machine-identity surface
// (product design §2.3/D5): one-time enrollment, short-TTL opaque access
// tokens, renewal, revocation, and the service-plane heartbeat records.
// Since the v1.5 topology split the surface lives behind the stateless
// edge access point (woca-edge): the RegistryHandler rides the service
// plane (woca-server's gateway listener), the ConfigHandler rides the
// config plane (woca-config); the bridge only ever learns the edge's
// address. The core-internal /gw/resolve and /gw/bridges routes are the
// edge's call-home validation hooks (R7 — the edge keeps no copy).
package entrygw

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/projanvil/woca/internal/platform/httpx"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/wocadb"
)

// AccessTTL bounds machine access tokens (design: 1–24h; short TTL is the
// worst-case leak window when a bridge is revoked while offline).
const AccessTTL = 6 * time.Hour

// EnrollmentTTL bounds one-time enrollment tokens (design: 30min–24h).
const EnrollmentTTL = 12 * time.Hour

// Store is the wocadb-backed machineid.Registry plus the service-plane
// heartbeat record.
type Store struct {
	db *wocadb.DB
}

// NewStore opens (and migrates) the gateway state on the shared database.
func NewStore(wdb *wocadb.DB) (*Store, error) {
	s := &Store{db: wdb}
	// Binary and timestamp columns are dialect vocabulary: BYTEA/BIGINT
	// on postgres, BLOB/INTEGER on sqlite (the sqlite test path never
	// hits the PG spellings — the dev compose stack caught both). The
	// timestamp columns hold unix milliseconds: int4 overflows at
	// 1970+49d, so postgres must use BIGINT (the same convention as
	// wocadb's own settings table).
	binCol, msCol := "BLOB", "INTEGER"
	alter := []string(nil)
	if wdb.Dialect == wocadb.DialectPostgres {
		binCol, msCol = "BYTEA", "BIGINT"
		// Existing stacks created these columns as int4 before the
		// convention landed — widen them (idempotent, no data loss:
		// every stored value predating 2004 fits either way).
		alter = []string{
			`ALTER TABLE gw_enrollments ALTER COLUMN expires_at TYPE BIGINT`,
			`ALTER TABLE gw_enrollments ALTER COLUMN used_at TYPE BIGINT`,
			`ALTER TABLE gw_bridges ALTER COLUMN created_at TYPE BIGINT`,
			`ALTER TABLE gw_bridges ALTER COLUMN revoked_at TYPE BIGINT`,
			`ALTER TABLE gw_access ALTER COLUMN expires_at TYPE BIGINT`,
			`ALTER TABLE gw_pending ALTER COLUMN created_at TYPE BIGINT`,
			`ALTER TABLE gw_heartbeats ALTER COLUMN at TYPE BIGINT`,
			`ALTER TABLE gw_audit ALTER COLUMN at TYPE BIGINT`,
		}
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_enrollments (
			token_hash TEXT PRIMARY KEY,
			expires_at %s NOT NULL,
			used_at %s
		)`, msCol, msCol),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_bridges (
			id TEXT PRIMARY KEY,
			pub_key %s NOT NULL,
			created_at %s NOT NULL,
			revoked_at %s
		)`, binCol, msCol, msCol),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_access (
			token_hash TEXT PRIMARY KEY,
			bridge_id TEXT NOT NULL,
			expires_at %s NOT NULL
		)`, msCol),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_pending (
			fingerprint TEXT PRIMARY KEY,
			public_key %s NOT NULL,
			created_at %s NOT NULL,
			approved_token TEXT
		)`, binCol, msCol),
		`CREATE TABLE IF NOT EXISTS gw_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_heartbeats (
			bridge_id TEXT PRIMARY KEY,
			at %s NOT NULL,
			status TEXT NOT NULL
		)`, msCol),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS gw_audit (
			at %s NOT NULL,
			event TEXT NOT NULL,
			bridge_id TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		)`, msCol),
	}
	stmts = append(stmts, alter...)
	// The registry tables are created by whichever core role boots
	// first against the shared database — server and config race here on
	// PG, and CREATE TABLE IF NOT EXISTS is not race-safe there (the same
	// pg_type collision wocadb's own migration locks out).
	if err := s.db.WithMigrateLock(func(q wocadb.Querier) error {
		for _, ddl := range stmts {
			if _, err := q.Exec(ddl); err != nil {
				return fmt.Errorf("entrygw: migrate: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ph(i int) string { return s.db.Dialect.Placeholder(i) }

// IssueEnrollment mints and records a one-time enrollment token. When
// the operator pinned the edge certificate (gw_settings edge_spki_pin),
// the pin rides inside the token — the stored hash covers the base
// credential only.
func (s *Store) IssueEnrollment(ctx context.Context, ttl time.Duration) (string, error) {
	base, err := machineid.NewEnrollmentToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(
		`INSERT INTO gw_enrollments (token_hash, expires_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`)`,
		machineid.HashToken(base), time.Now().Add(ttl).UnixMilli())
	if err != nil {
		return "", fmt.Errorf("entrygw: issue enrollment: %w", err)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		return "", err
	}
	return machineid.AttachEnrollmentPin(base, settings[SettingKeyEdgeSPKIPin]), nil
}

// ConsumeEnrollment implements machineid.Registry.
func (s *Store) ConsumeEnrollment(ctx context.Context, tokenHash string, pub []byte, _ time.Duration) (string, error) {
	var expires int64
	err := s.db.QueryRow(`SELECT expires_at FROM gw_enrollments WHERE token_hash = `+s.ph(0), tokenHash).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && time.Now().UnixMilli() > expires) {
		// Compare anyway — no oracle about which part failed.
		return "", machineid.ErrEnrollment
	}
	if err != nil {
		return "", fmt.Errorf("entrygw: enrollment lookup: %w", err)
	}
	res, err := s.db.Exec(`UPDATE gw_enrollments SET used_at = `+s.ph(0)+` WHERE token_hash = `+s.ph(1)+` AND used_at IS NULL`,
		time.Now().UnixMilli(), tokenHash)
	if err != nil {
		return "", fmt.Errorf("entrygw: enrollment consume: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", machineid.ErrEnrollment // reused or raced out
	}
	raw := make([]byte, 4)
	id, err := randomID("br", raw)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT INTO gw_bridges (id, pub_key, created_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`)`,
		id, pub, time.Now().UnixMilli()); err != nil {
		return "", fmt.Errorf("entrygw: register bridge: %w", err)
	}
	return id, nil
}

// IssueAccess implements machineid.Registry.
func (s *Store) IssueAccess(ctx context.Context, bridgeID string, ttl time.Duration) (string, int64, error) {
	if err := s.bridgeLive(ctx, bridgeID); err != nil {
		return "", 0, err
	}
	token, err := machineid.NewAccessToken()
	if err != nil {
		return "", 0, err
	}
	exp := time.Now().Add(ttl).UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO gw_access (token_hash, bridge_id, expires_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`)`,
		machineid.HashToken(token), bridgeID, exp); err != nil {
		return "", 0, fmt.Errorf("entrygw: issue access: %w", err)
	}
	return token, exp, nil
}

// ResolveAccess implements machineid.Registry.
func (s *Store) ResolveAccess(_ context.Context, tokenHash string) (string, error) {
	var (
		bridgeID string
		expires  int64
	)
	err := s.db.QueryRow(`SELECT bridge_id, expires_at FROM gw_access WHERE token_hash = `+s.ph(0), tokenHash).
		Scan(&bridgeID, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && time.Now().UnixMilli() > expires) {
		return "", machineid.ErrUnknown
	}
	if err != nil {
		return "", fmt.Errorf("entrygw: access lookup: %w", err)
	}
	if err := s.bridgeLive(context.Background(), bridgeID); err != nil {
		return "", err
	}
	return bridgeID, nil
}

// RenewAccess implements machineid.Registry.
func (s *Store) RenewAccess(ctx context.Context, tokenHash string, ttl time.Duration) (string, int64, error) {
	bridgeID, err := s.ResolveAccess(ctx, tokenHash)
	if err != nil {
		return "", 0, err
	}
	if _, err := s.db.Exec(`DELETE FROM gw_access WHERE token_hash = `+s.ph(0), tokenHash); err != nil {
		return "", 0, fmt.Errorf("entrygw: renew delete: %w", err)
	}
	return s.IssueAccess(ctx, bridgeID, ttl)
}

// RevokeBridge implements machineid.Registry.
func (s *Store) RevokeBridge(_ context.Context, bridgeID string) error {
	if _, err := s.db.Exec(`UPDATE gw_bridges SET revoked_at = `+s.ph(0)+` WHERE id = `+s.ph(1),
		time.Now().UnixMilli(), bridgeID); err != nil {
		return fmt.Errorf("entrygw: revoke: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM gw_access WHERE bridge_id = `+s.ph(0), bridgeID); err != nil {
		return fmt.Errorf("entrygw: revoke cascade: %w", err)
	}
	return nil
}

// SubmitEnrollmentRequest registers a worker-led enrollment (product
// design §2.3 备选注册 UX): the bridge prints its key fingerprint and an
// administrator approves it on the console — zero pre-shared secrets.
func (s *Store) SubmitEnrollmentRequest(_ context.Context, pub []byte) (string, error) {
	sum := sha256.Sum256(pub)
	fp := hex.EncodeToString(sum[:])[:16]
	if _, err := s.db.Exec(
		`INSERT INTO gw_pending (fingerprint, public_key, created_at) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`)
		 ON CONFLICT(fingerprint) DO UPDATE SET public_key = excluded.public_key`,
		fp, pub, time.Now().UnixMilli()); err != nil {
		return "", fmt.Errorf("entrygw: enrollment request: %w", err)
	}
	return fp, nil
}

// PendingRequest is one worker-led enrollment awaiting approval.
type PendingRequest struct {
	Fingerprint string `json:"fingerprint"`
	CreatedAt   int64  `json:"created_at"`
}

// PendingRequests lists requests awaiting approval (admin console).
func (s *Store) PendingRequests(_ context.Context) ([]PendingRequest, error) {
	rows, err := s.db.Query(`SELECT fingerprint, created_at FROM gw_pending WHERE approved_token IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRequest
	for rows.Next() {
		var p PendingRequest
		if err := rows.Scan(&p.Fingerprint, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApproveEnrollment mints a one-time token for a pending request; the
// bridge's approval poll picks it up and completes the normal enrollment.
func (s *Store) ApproveEnrollment(ctx context.Context, fingerprint string) (string, error) {
	var ok bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM gw_pending WHERE fingerprint = `+s.ph(0)+`)`,
		fingerprint).Scan(&ok); err != nil || !ok {
		if err != nil {
			return "", err
		}
		return "", machineid.ErrEnrollment
	}
	token, err := s.IssueEnrollment(ctx, EnrollmentTTL)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`UPDATE gw_pending SET approved_token = `+s.ph(0)+` WHERE fingerprint = `+s.ph(1),
		token, fingerprint); err != nil {
		return "", err
	}
	return token, nil
}

// PollApproval returns the approved enrollment token for a fingerprint
// (ok=false until the administrator approves).
func (s *Store) PollApproval(_ context.Context, fingerprint string) (string, bool, error) {
	var token sql.NullString
	err := s.db.QueryRow(`SELECT approved_token FROM gw_pending WHERE fingerprint = `+s.ph(0), fingerprint).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !token.Valid || token.String == "" {
		return "", false, nil
	}
	// One delivery: the token is single-use anyway; clearing keeps the
	// pending list tidy.
	_, _ = s.db.Exec(`DELETE FROM gw_pending WHERE fingerprint = `+s.ph(0), fingerprint)
	return token.String, true, nil
}

// SettingKeyAllowInsecure toggles development posture: bridges may
// connect without a valid certificate chain (self-signed / plain TLS).
// It exists for 联调 — production gateways keep it off (R6).
const SettingKeyAllowInsecure = "allow_insecure_bridges"

// SettingKeyEdgeSPKIPin carries the edge access point's certificate SPKI
// digest (hex). When set, issued enrollment tokens embed it (k3s mode,
// design §2.3): bridges verify the edge's identity before spending the
// one-time credential — a poisoned URL cannot harvest enrollments.
const SettingKeyEdgeSPKIPin = "edge_spki_pin"

// Settings returns the gateway policy settings.
func (s *Store) Settings(_ context.Context) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM gw_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting upserts one gateway policy setting.
func (s *Store) SetSetting(_ context.Context, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO gw_settings (key, value) VALUES (`+s.ph(0)+`, `+s.ph(1)+`)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AllowInsecure reports whether certificate-less bridge connections are
// permitted (development posture).
func (s *Store) AllowInsecure(ctx context.Context) bool {
	settings, err := s.Settings(ctx)
	if err != nil {
		return false
	}
	return settings[SettingKeyAllowInsecure] == "true"
}

// Heartbeat records a service-plane heartbeat.
func (s *Store) Heartbeat(_ context.Context, bridgeID string, status json.RawMessage) error {
	if _, err := s.db.Exec(
		`INSERT INTO gw_heartbeats (bridge_id, at, status) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`)
		 ON CONFLICT(bridge_id) DO UPDATE SET at = excluded.at, status = excluded.status`,
		bridgeID, time.Now().UnixMilli(), string(status)); err != nil {
		return fmt.Errorf("entrygw: heartbeat: %w", err)
	}
	return nil
}

// BridgeLiveAfter is the heartbeat freshness window: bridges beat every
// 30s, so a beat within 3 intervals counts as live.
const BridgeLiveAfter = int64(90_000)

// BridgeInfo is one row of the registration inventory.
type BridgeInfo struct {
	BridgeID  string `json:"bridge_id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	Live      bool   `json:"live"`
}

// ListBridges returns the registered, unrevoked bridges with heartbeat
// freshness — the edge's selection inventory (v1.6 入口②).
func (s *Store) ListBridges(_ context.Context) ([]BridgeInfo, error) {
	rows, err := s.db.Query(`
		SELECT b.id, b.created_at, COALESCE(h.at, 0)
		FROM gw_bridges b LEFT JOIN gw_heartbeats h ON h.bridge_id = b.id
		WHERE b.revoked_at IS NULL ORDER BY b.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UnixMilli()
	var out []BridgeInfo
	for rows.Next() {
		var b BridgeInfo
		if err := rows.Scan(&b.BridgeID, &b.CreatedAt, &b.LastSeen); err != nil {
			return nil, err
		}
		b.Live = b.LastSeen != 0 && now-b.LastSeen < BridgeLiveAfter
		out = append(out, b)
	}
	return out, rows.Err()
}

// GwAuditRow is one service-plane governance event (R5).
type GwAuditRow struct {
	At       int64  `json:"at"`
	Event    string `json:"event"`
	BridgeID string `json:"bridge_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// AuditEvent appends one governance event (bridge.enrolled /
// bridge.revoked / enrollment.issued / enrollment.approved / …).
func (s *Store) AuditEvent(_ context.Context, event, bridgeID, detail string) {
	_, _ = s.db.Exec(
		`INSERT INTO gw_audit (at, event, bridge_id, detail) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`)`,
		time.Now().UnixMilli(), event, bridgeID, detail)
}

// AuditEvents returns the newest governance events.
// AuditFilter narrows the governance trail: an exact bridge id, an event
// name, and a [From, To] millisecond window (zero bounds are open).
type AuditFilter struct {
	BridgeID string
	Event    string
	From     int64
	To       int64
}

// AuditEvents returns one page of the governance trail (newest first)
// matching the filter. limit/offset page at the database; total counts
// the filtered set.
func (s *Store) AuditEvents(_ context.Context, f AuditFilter, limit, offset int) ([]GwAuditRow, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where, args := s.auditWhere(f)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gw_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT at, event, bridge_id, detail FROM gw_audit`+where+
		` ORDER BY at DESC LIMIT `+strconv.Itoa(limit)+` OFFSET `+strconv.Itoa(offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []GwAuditRow
	for rows.Next() {
		var r GwAuditRow
		if err := rows.Scan(&r.At, &r.Event, &r.BridgeID, &r.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// AuditEventAt appends one governance event with an explicit timestamp
// (test seeding; production stamps with the wall clock).
func (s *Store) AuditEventAt(_ context.Context, at int64, event, bridgeID, detail string) {
	_, _ = s.db.Exec(
		`INSERT INTO gw_audit (at, event, bridge_id, detail) VALUES (`+s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`)`,
		at, event, bridgeID, detail)
}

// AuditEventNames lists the distinct event names for the console's
// filter dropdown.
func (s *Store) AuditEventNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT event FROM gw_audit ORDER BY event`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// auditWhere builds the shared WHERE clause for count and page queries.
func (s *Store) auditWhere(f AuditFilter) (string, []any) {
	conds := []string{}
	var args []any
	add := func(cond string, v any) {
		conds = append(conds, cond)
		args = append(args, v)
	}
	if f.BridgeID != "" {
		// Fuzzy on the bridge id (the console searches by name fragment).
		add("LOWER(bridge_id) LIKE "+s.ph(len(args)), "%"+strings.ToLower(f.BridgeID)+"%")
	}
	if f.Event != "" {
		add("event = "+s.ph(len(args)), f.Event)
	}
	if f.From > 0 {
		add("at >= "+s.ph(len(args)), f.From)
	}
	if f.To > 0 {
		add("at <= "+s.ph(len(args)), f.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *Store) bridgeLive(_ context.Context, bridgeID string) error {
	var revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT revoked_at FROM gw_bridges WHERE id = `+s.ph(0), bridgeID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return machineid.ErrRevoked
	}
	if err != nil {
		return fmt.Errorf("entrygw: bridge lookup: %w", err)
	}
	if revoked.Valid {
		return machineid.ErrRevoked
	}
	return nil
}

// ConfigSource supplies the config-plane payload for a bridge: a monotonic
// version and the managed settings map (key → value; secret keys included
// — they travel bridge↔gateway only, never to clients, per D5).
type ConfigSource func(ctx context.Context, bridgeID string) (version string, settings map[string]string, err error)

// gwBase carries the shared machine-token authentication over the store.
type gwBase struct {
	reg   machineid.Registry
	store *Store
}

// RegistryHandler is the service-plane machine surface (product design
// v1.5 拓扑拆分): enrollment, renewal, heartbeats, plus the core-internal
// resolve/bridges routes the edge access point calls back over the private
// network (R7: the edge keeps no registry copy — every check comes home).
// Mounted on woca-server's gateway listener.
type RegistryHandler struct{ gwBase }

// NewRegistryHandler builds the service-plane surface over its store.
func NewRegistryHandler(store *Store) *RegistryHandler {
	return &RegistryHandler{gwBase{reg: store, store: store}}
}

// RegisterRoutes mounts the registry /gw/* routes.
func (h *RegistryHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /gw/enroll", h.handleEnroll)
	mux.HandleFunc("POST /gw/enroll-request", h.handleEnrollRequest)
	mux.HandleFunc("GET /gw/enroll-status", h.handleEnrollStatus)
	// hello is the anonymous posture probe: bridges and operators check
	// the gateway's TLS expectations before enrolling.
	mux.HandleFunc("GET /gw/hello", h.handleHello)
	mux.HandleFunc("POST /gw/renew", h.auth(h.handleRenew))
	mux.HandleFunc("POST /gw/heartbeat", h.auth(h.handleHeartbeat))
	// Core-internal (edge → service plane): token-hash resolution and the
	// live bridge inventory for the edge's selection page. These ride the
	// private network only — never forwarded from the public listener.
	mux.HandleFunc("POST /gw/resolve", h.handleResolve)
	mux.HandleFunc("GET /gw/bridges", h.handleBridges)
}

// ConfigHandler is the config-plane machine surface: the /gw/config pull
// with its own access-token check (the config plane reads the shared
// registry tables — no cross-service call needed).
type ConfigHandler struct {
	gwBase
	cfg ConfigSource
}

// NewConfigHandler builds the config-plane pull surface.
func NewConfigHandler(store *Store, cfg ConfigSource) *ConfigHandler {
	return &ConfigHandler{gwBase: gwBase{reg: store, store: store}, cfg: cfg}
}

// RegisterRoutes mounts the config-plane /gw/* routes.
func (h *ConfigHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /gw/config", h.auth(h.handleConfig))
}

// RouteAuth wraps a config-plane /gw route with the machine access-token
// check — for routes the config plane owns beyond the config pull (each
// still resolves the caller's bridge identity against the shared
// registry).
func (h *ConfigHandler) RouteAuth(next http.HandlerFunc) http.HandlerFunc {
	return h.auth(next)
}

// handleResolve maps a hashed access token to its bridge id — the
// stateless edge's authentication callback (R7). Hashes only: no token
// material ever crosses this route.
func (h *RegistryHandler) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TokenHash string `json:"token_hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		len(req.TokenHash) != 64 {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	bridgeID, err := h.reg.ResolveAccess(r.Context(), req.TokenHash)
	if err != nil {
		writeGW(w, http.StatusNotFound, "unknown")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bridge_id": bridgeID})
}

// handleBridges lists the registered, unrevoked bridges with heartbeat
// freshness — the edge's target-selection inventory and the console's
// registration view.
func (h *RegistryHandler) handleBridges(w http.ResponseWriter, r *http.Request) {
	bridges, err := h.store.ListBridges(r.Context())
	if err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, bridges)
}

type enrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	PublicKey       []byte `json:"public_key"`
}

// handleEnroll consumes a one-time enrollment token and registers the
// machine's public key (the only anonymous /gw route).
func (h *RegistryHandler) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil ||
		!machineid.ValidEnrollment(req.EnrollmentToken) || len(req.PublicKey) == 0 || len(req.PublicKey) > 512 {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, err := machineid.ParsePublicKeyPEM(req.PublicKey); err != nil {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	bridgeID, err := h.reg.ConsumeEnrollment(r.Context(), machineid.HashToken(req.EnrollmentToken), req.PublicKey, EnrollmentTTL)
	if err != nil {
		writeGW(w, http.StatusUnauthorized, "enrollment rejected")
		return
	}
	token, exp, err := h.reg.IssueAccess(r.Context(), bridgeID, AccessTTL)
	if err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	h.store.AuditEvent(r.Context(), "bridge.enrolled", bridgeID, r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"bridge_id": bridgeID, "access_token": token, "access_expires_at": exp,
	})
}

// handleHello reports the gateway's connection posture (anonymous): the
// service identity plus whether certificate-less bridge connections are
// allowed (development mode).
func (h *RegistryHandler) handleHello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        "woca-entrygw",
		"allow_insecure": h.store.AllowInsecure(context.Background()),
	})
}

// handleEnrollRequest is the worker-led flow: register the machine key,
// print the fingerprint, wait for the console approval.
func (h *RegistryHandler) handleEnrollRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicKey []byte `json:"public_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil ||
		len(req.PublicKey) == 0 || len(req.PublicKey) > 512 {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, err := machineid.ParsePublicKeyPEM(req.PublicKey); err != nil {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	fp, err := h.store.SubmitEnrollmentRequest(r.Context(), req.PublicKey)
	if err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"fingerprint": fp})
}

// handleEnrollStatus polls for the console approval (202 until approved).
func (h *RegistryHandler) handleEnrollStatus(w http.ResponseWriter, r *http.Request) {
	token, ok, err := h.store.PollApproval(r.Context(), r.URL.Query().Get("fingerprint"))
	if err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeGW(w, http.StatusAccepted, "pending approval")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"enrollment_token": token})
}

func (h *RegistryHandler) handleRenew(w http.ResponseWriter, r *http.Request) {
	token, exp, err := h.reg.RenewAccess(r.Context(), machineid.HashToken(bearer(r)), AccessTTL)
	if err != nil {
		writeGW(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "access_expires_at": exp})
}

// handleConfig serves the config-plane payload; If-None-Match style
// versioning: an unchanged version yields 304 with an empty body (the
// bridge must never treat that as "clear the cache" — §3.2). A ?wait=
// duration turns the request into a blocking query (Syncthing/Consul
// style, product design §2.3): the response parks until the version
// moves or the wait elapses, so a push becomes "the next poll returns
// immediately".
func (h *ConfigHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	bridgeID := bridgeIDFromCtx(r.Context())
	known := r.URL.Query().Get("version")
	wait := parseWait(r.URL.Query().Get("wait"))

	version, settings, err := h.cfg(r.Context(), bridgeID)
	if err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if known != "" && known == version {
		if wait == 0 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		deadline := time.Now().Add(wait)
		for known == version {
			if r.Context().Err() != nil {
				return
			}
			if time.Now().After(deadline) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			time.Sleep(400 * time.Millisecond)
			if version, settings, err = h.cfg(r.Context(), bridgeID); err != nil {
				writeGW(w, http.StatusInternalServerError, "unavailable")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "settings": settings})
}

// parseWait bounds the blocking-query duration (max 60s, min 1s).
func parseWait(v string) time.Duration {
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second {
		return 0
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func (h *RegistryHandler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	body := json.RawMessage{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil {
		writeGW(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := h.store.Heartbeat(r.Context(), bridgeIDFromCtx(r.Context()), body); err != nil {
		writeGW(w, http.StatusInternalServerError, "unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// auth wraps a /gw route with the machine access-token check.
func (h *gwBase) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeGW(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		bridgeID, err := h.reg.ResolveAccess(r.Context(), machineid.HashToken(token))
		if err != nil {
			writeGW(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(withBridgeID(r.Context(), bridgeID)))
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

type bridgeIDKey struct{}

func withBridgeID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, bridgeIDKey{}, id)
}

func bridgeIDFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(bridgeIDKey{}).(string)
	return id
}

// randomID mints a short random identifier with the given prefix.
func randomID(prefix string, raw []byte) (string, error) {
	if _, err := cryptorand.Read(raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%x", prefix, raw), nil
}

func writeGW(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}
