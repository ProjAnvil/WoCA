package opsconsole

// Tests cover the tenant ops console contract (product design v1.7 §6.7):
// the organization-credential lifecycle (issue shows the key exactly once,
// revoke marks the row), last-use folding from bridge heartbeats, the
// enrollment/bridge/audit governance surface, the gateway posture seeded
// from the startup environment, and the console credential gate.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

type fixture struct {
	svc  *Service
	gw   *entrygw.Store
	auth *authcore.Service
	keys *tenant.Store
	srv  *httptest.Server
	db   *wocadb.DB
	st   *settings.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := wocadb.Open("sqlite:" + filepath.Join(dir, "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := settings.Open("sqlite:" + filepath.Join(dir, "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gw, err := entrygw.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := authcore.New(keys, db, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, gw, auth, keys, zerolog.Nop())
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return &fixture{svc: svc, gw: gw, auth: auth, keys: keys, srv: srv, db: db, st: st}
}

func (f *fixture) do(method, path string, body any) (*http.Response, []byte) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

// TestUserLifecycle: issue (woca_ key shown exactly once) → list
// (prefix, remark, last_used) → revoke (row marked, sessions killed,
// second revoke rejected).
func TestUserLifecycle(t *testing.T) {
	f := newFixture(t)

	resp, raw := f.do("POST", "/api/tenant/users", map[string]string{"remark": "alice", "role": "member"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue = %d %s", resp.StatusCode, raw)
	}
	var issued map[string]any
	_ = json.Unmarshal(raw, &issued)
	key, _ := issued["key"].(string)
	if !strings.HasPrefix(key, "woca_") {
		t.Fatalf("issued key shape: %q", key)
	}

	// Login through the core auth service, then the session shows up in
	// the console's session inventory and last_used is authoritative.
	if _, err := f.auth.Login(t.Context(), key); err != nil {
		t.Fatal(err)
	}

	resp, raw = f.do("GET", "/api/tenant/users", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	var usersDoc struct {
		Items []userRow `json:"items"`
		Total int       `json:"total"`
	}
	if err := json.Unmarshal(raw, &usersDoc); err != nil {
		t.Fatalf("list decode: %v (%s)", err, raw)
	}
	users := usersDoc.Items
	if len(users) != 1 || users[0].Remark != "alice" || users[0].Role != "member" {
		t.Fatalf("users = %+v", users)
	}
	if users[0].Prefix == "" {
		t.Fatal("no prefix in list")
	}
	if users[0].LastUsed == 0 {
		t.Fatalf("core-side last_used missing: %+v", users[0])
	}

	// The session inventory lists the live session.
	resp, raw = f.do("GET", "/api/tenant/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions = %d", resp.StatusCode)
	}
	var sessDoc struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.Unmarshal(raw, &sessDoc)
	if len(sessDoc.Items) != 1 || sessDoc.Total != 1 || sessDoc.Items[0]["remark"] != "alice" {
		t.Fatalf("sessions = %v %d", sessDoc.Items, sessDoc.Total)
	}

	resp, raw = f.do("DELETE", "/api/tenant/users/"+users[0].Prefix, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d %s", resp.StatusCode, raw)
	}
	// Revoking again is a client error, not a silent success.
	resp, _ = f.do("DELETE", "/api/tenant/users/"+users[0].Prefix, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("double revoke = %d want 400", resp.StatusCode)
	}
	// The key's session died with the key.
	_, raw = f.do("GET", "/api/tenant/sessions", nil)
	var sessAfter struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(raw, &sessAfter)
	if len(sessAfter.Items) != 1 || sessAfter.Items[0]["revoked_at"] == float64(0) {
		t.Fatalf("session not revoked with the key: %v", sessAfter.Items)
	}

	// The governance trail carries both events.
	resp, raw = f.do("GET", "/api/gw/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d", resp.StatusCode)
	}
	if !strings.Contains(string(raw), "user.issued") || !strings.Contains(string(raw), "user.revoked") {
		t.Fatalf("audit trail missing user events: %s", raw)
	}
}

// TestEnrollmentAndBridgeInventory: token issuance lands in the audit
// trail; the registry inventory lists the bridge with heartbeat
// freshness; revocation removes it.
func TestEnrollmentAndBridgeInventory(t *testing.T) {
	f := newFixture(t)

	resp, raw := f.do("POST", "/api/gw/enrollments", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue enrollment = %d %s", resp.StatusCode, raw)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if token, _ := doc["enrollment_token"].(string); token == "" {
		t.Fatalf("no token: %s", raw)
	}

	// Enroll a bridge through the store (the machine path), then check
	// the inventory reflects it.
	priv, err := machineid.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, err := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	bridgeID, err := f.gw.ConsumeEnrollment(t.Context(), machineid.HashToken(doc["enrollment_token"].(string)), pubPEM, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, raw = f.do("GET", "/api/gw/bridges", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridges = %d", resp.StatusCode)
	}
	if !strings.Contains(string(raw), bridgeID) {
		t.Fatalf("inventory missing bridge: %s", raw)
	}

	resp, _ = f.do("DELETE", "/api/gw/bridges/"+bridgeID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke bridge = %d", resp.StatusCode)
	}
	resp, raw = f.do("GET", "/api/gw/bridges", nil)
	if strings.Contains(string(raw), bridgeID) {
		t.Fatalf("revoked bridge still listed: %s", raw)
	}
}

// TestGatewayPostureSeededFromEnv: the certificate-less posture is a
// startup-env decision (R6), seeded at boot, and never appears on the
// console's settings API.
func TestGatewayPostureSeededFromEnv(t *testing.T) {
	t.Setenv("WOCA_INSECURE_BRIDGES", "")
	f := newFixture(t)
	if f.gw.AllowInsecure(t.Context()) {
		t.Fatal("insecure posture on without the env")
	}
	resp, raw := f.do("GET", "/api/gw/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gw settings = %d", resp.StatusCode)
	}
	if strings.Contains(string(raw), "allow_insecure") {
		t.Fatalf("settings API leaks the posture knob: %s", raw)
	}

	t.Setenv("WOCA_INSECURE_BRIDGES", "1")
	New(f.st, f.gw, f.auth, f.keys, zerolog.Nop())
	if !f.gw.AllowInsecure(t.Context()) {
		t.Fatal("env did not enable the posture at boot")
	}
}

// TestEdgePinValidation: the SPKI pin accepts a 64-char hex digest or
// empty, and rejects everything else.
func TestEdgePinValidation(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.do("PUT", "/api/gw/settings", map[string]string{"edge_spki_pin": "zz"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pin = %d want 400", resp.StatusCode)
	}
	pin := strings.Repeat("ab", 32)
	resp, _ = f.do("PUT", "/api/gw/settings", map[string]string{"edge_spki_pin": pin})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good pin = %d", resp.StatusCode)
	}
	settings, _ := f.gw.Settings(t.Context())
	if settings[entrygw.SettingKeyEdgeSPKIPin] != pin {
		t.Fatalf("pin not stored: %+v", settings)
	}
}

// TestConsoleGate: with credentials configured every /api route sits
// behind the session cookie; the login exchange mints it.
func TestConsoleGate(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	f := newFixture(t)

	resp, _ := f.do("GET", "/api/tenant/users", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %d want 401", resp.StatusCode)
	}
	resp, _ = f.do("POST", "/api/console/login", map[string]string{"username": "admin", "password": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d want 401", resp.StatusCode)
	}

	resp, _ = f.do("POST", "/api/console/login", map[string]string{"username": "admin", "password": "s3cret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	// The cookie jar replays the session cookie.
	for _, c := range resp.Cookies() {
		if c.Name == "woca_console_session" && c.Value != "" {
			req, _ := http.NewRequest("GET", f.srv.URL+"/api/tenant/users", nil)
			req.AddCookie(c)
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("with session = %d want 200", r.StatusCode)
			}
			return
		}
	}
	t.Fatal("no session cookie issued")
}

// TestTenantSearchAndPaging: ?q= filters at the database and
// limit/offset page through the result set.
func TestTenantSearchAndPaging(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"alice", "alina", "bob"} {
		resp, _ := f.do("POST", "/api/tenant/users", map[string]string{"remark": name, "role": "member"})
		resp.Body.Close()
	}

	// Search: "ali" matches alice+alina, not bob.
	_, raw := f.do("GET", "/api/tenant/users?q=ali", nil)
	var doc struct {
		Items []userRow `json:"items"`
		Total int       `json:"total"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 2 || len(doc.Items) != 2 {
		t.Fatalf("search = %+v total=%d", doc.Items, doc.Total)
	}

	// Paging: page size 1 over the full set of 3.
	_, raw = f.do("GET", "/api/tenant/users?limit=1&offset=0", nil)
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 3 || len(doc.Items) != 1 {
		t.Fatalf("page1 = %+v total=%d", doc.Items, doc.Total)
	}
	_, raw = f.do("GET", "/api/tenant/users?limit=1&offset=3", nil)
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 3 || len(doc.Items) != 0 {
		t.Fatalf("past-end page = %+v total=%d", doc.Items, doc.Total)
	}

	// The q also matches the key PREFIX (fuzzy). Fetch the full first
	// page to find alice's prefix (doc currently holds a 1-item page).
	_, raw = f.do("GET", "/api/tenant/users?limit=50", nil)
	_ = json.Unmarshal(raw, &doc)
	prefix := ""
	for _, u := range doc.Items {
		if u.Remark == "alice" {
			prefix = u.Prefix
		}
	}
	_, raw = f.do("GET", "/api/tenant/users?q="+prefix[:4], nil)
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 1 {
		t.Fatalf("prefix search = total %d", doc.Total)
	}

	// Ordering: issued_at_desc puts the newest first (all three were
	// issued in the same test; assert shape + deterministic ordering).
	_, raw = f.do("GET", "/api/tenant/users?order=issued_desc", nil)
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 3 || len(doc.Items) != 3 {
		t.Fatalf("ordered = %+v total=%d", doc.Items, doc.Total)
	}
	if doc.Items[0].IssuedAt < doc.Items[2].IssuedAt {
		t.Fatalf("desc order broken: %v < %v", doc.Items[0].IssuedAt, doc.Items[2].IssuedAt)
	}

	// Sessions pager has the same contract.
	_, raw = f.do("GET", "/api/tenant/sessions?q=zzz&limit=10&offset=0", nil)
	var sessDoc struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.Unmarshal(raw, &sessDoc)
	if sessDoc.Total != 0 || len(sessDoc.Items) != 0 {
		t.Fatalf("session search = %+v total=%d", sessDoc.Items, sessDoc.Total)
	}
}

// TestAuditFilters: bridge/event/time filters and paging ride the
// database query; the event vocabulary endpoint feeds the dropdown.
func TestAuditFilters(t *testing.T) {
	f := newFixture(t)

	// Seed a varied trail: two events, two bridges, distinct times.
	seed := []struct {
		at            int64
		event, bridge string
	}{
		{1000, "bridge.enrolled", "br_a"},
		{2000, "bridge.enrolled", "br_b"},
		{3000, "user.issued", ""},
		{4000, "user.revoked", ""},
	}
	for _, e := range seed {
		f.gw.AuditEventAt(t.Context(), e.at, e.event, e.bridge, "seed")
	}

	get := func(path string) (struct {
		Items []entrygw.GwAuditRow `json:"items"`
		Total int                  `json:"total"`
	}, []byte) {
		resp, raw := f.do("GET", path, nil)
		resp.Body.Close()
		var doc struct {
			Items []entrygw.GwAuditRow `json:"items"`
			Total int                  `json:"total"`
		}
		_ = json.Unmarshal(raw, &doc)
		return doc, raw
	}

	if doc, _ := get("/api/gw/audit?bridge=br_a"); doc.Total != 1 || doc.Items[0].BridgeID != "br_a" {
		t.Fatalf("bridge filter = %+v total=%d", doc.Items, doc.Total)
	}
	if doc, _ := get("/api/gw/audit?event=bridge.enrolled"); doc.Total != 2 {
		t.Fatalf("event filter total=%d", doc.Total)
	}
	if doc, _ := get("/api/gw/audit?from=2500"); doc.Total != 2 {
		t.Fatalf("from filter total=%d", doc.Total)
	}
	if doc, _ := get("/api/gw/audit?to=2500"); doc.Total != 2 {
		t.Fatalf("to filter total=%d", doc.Total)
	}
	if doc, _ := get("/api/gw/audit?from=1500&to=3500"); doc.Total != 2 {
		t.Fatalf("window filter total=%d", doc.Total)
	}
	if doc, _ := get("/api/gw/audit?event=user.issued&from=1500&to=3500"); doc.Total != 1 {
		t.Fatalf("combined filter total=%d", doc.Total)
	}
	// Paging.
	if doc, _ := get("/api/gw/audit?limit=2&offset=2"); doc.Total != 4 || len(doc.Items) != 2 {
		t.Fatalf("page = %d items=%d total=%d", 3, len(doc.Items), doc.Total)
	}

	// Event vocabulary.
	resp, raw := f.do("GET", "/api/gw/audit/events", nil)
	resp.Body.Close()
	var names []string
	_ = json.Unmarshal(raw, &names)
	if len(names) != 3 {
		t.Fatalf("event names = %v", names)
	}
}

// TestMultiTenantLifecycle: tenants are business units — create one,
// issue a key under it, list/search by tenant name, and the per-tenant
// stats aggregate the counts.
func TestMultiTenantLifecycle(t *testing.T) {
	f := newFixture(t)

	// No tenants gate: issuing without a tenant falls back to the
	// platform circle; with an unknown tenant it is refused.
	resp, _ := f.do("POST", "/api/tenant/users", map[string]string{"remark": "orphan", "role": "member", "tenant_id": "t_nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown tenant issue = %d want 400", resp.StatusCode)
	}

	// Create a business unit.
	resp, raw := f.do("POST", "/api/tenant/tenants", map[string]string{"name": "Platform Team"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create tenant = %d %s", resp.StatusCode, raw)
	}
	var tn tenantRow
	_ = json.Unmarshal(raw, &tn)
	if tn.ID == "" || tn.Name != "Platform Team" {
		t.Fatalf("tenant = %+v", tn)
	}

	// Issue a key under it.
	resp, raw = f.do("POST", "/api/tenant/users", map[string]string{"remark": "alice", "role": "member", "tenant_id": tn.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue under tenant = %d %s", resp.StatusCode, raw)
	}

	// The list shows the tenant; q matches the tenant NAME.
	_, raw = f.do("GET", "/api/tenant/users?q="+url.QueryEscape("platform team"), nil)
	var doc struct {
		Items []userRow `json:"items"`
		Total int       `json:"total"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 1 || len(doc.Items) != 1 || doc.Items[0].TenantID != tn.ID || doc.Items[0].TenantName != "Platform Team" {
		t.Fatalf("tenant search = %+v total=%d", doc.Items, doc.Total)
	}

	// The tenant filter narrows to one unit.
	_, raw = f.do("GET", "/api/tenant/users?tenant="+tn.ID, nil)
	_ = json.Unmarshal(raw, &doc)
	if doc.Total != 1 {
		t.Fatalf("tenant filter total=%d", doc.Total)
	}

	// Stats aggregate per unit (login to create a session first).
	var issued struct {
		Key string `json:"key"`
	}
	_, raw = f.do("POST", "/api/tenant/users", map[string]string{"remark": "bob", "role": "member", "tenant_id": tn.ID})
	_ = json.Unmarshal(raw, &issued)
	if _, err := f.auth.Login(t.Context(), issued.Key); err != nil {
		t.Fatal(err)
	}
	_, raw = f.do("GET", "/api/tenant/stats", nil)
	var stats []tenantStats
	_ = json.Unmarshal(raw, &stats)
	var found *tenantStats
	for i := range stats {
		if stats[i].TenantID == tn.ID {
			found = &stats[i]
		}
	}
	if found == nil || found.KeysActive != 2 || found.Sessions != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	// Sessions list carries the tenant and q matches its name.
	_, raw = f.do("GET", "/api/tenant/sessions?q=platform", nil)
	var sessDoc struct {
		Items []struct {
			TenantID   string `json:"tenant_id"`
			TenantName string `json:"tenant_name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(raw, &sessDoc)
	if sessDoc.Total != 1 || sessDoc.Items[0].TenantName != "Platform Team" {
		t.Fatalf("session tenant search = %+v total=%d", sessDoc.Items, sessDoc.Total)
	}
}
