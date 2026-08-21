package configsvc

// Tests cover the management-plane contract: admin-key auth (401 without,
// 403 for member keys), the settings GET/PUT round trip with validation,
// export/import idempotence, the SPA fallback, the distribution-status
// inventory, and the per-subsystem postgres fields.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

type fixture struct {
	svc       *Service
	srv       *httptest.Server
	st        *settings.Store
	adminKey  string
	memberKey string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := wocadb.Open("sqlite:" + filepath.Join(dir, "cfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := settings.Open("sqlite:" + filepath.Join(dir, "cfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, ts, db, nil, zerolog.Nop())
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	f := &fixture{svc: svc, srv: srv, st: st}
	// The local platform tenant exists by default with user u_local; add
	// admin + member users and issue their keys (member lacks the settings
	// scopes — the role gate).
	au, err := ts.CreateUser(t.Context(), "local", "cfg-admin", "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	mu, err := ts.CreateUser(t.Context(), "local", "cfg-member", "member")
	if err != nil {
		t.Fatal(err)
	}
	f.adminKey = f.issueKey(t, "local", au.ID, "tenant-admin")
	f.memberKey = f.issueKey(t, "local", mu.ID, "member")
	return f
}

func (f *fixture) issueKey(t *testing.T, tenantID, userID, role string) string {
	t.Helper()
	_, secret, err := f.svc.tenants.IssueKey(t.Context(), tenantID, userID, "test", role)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func (f *fixture) do(method, path, key string, body any) *http.Response {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func TestAuthPosture(t *testing.T) {
	f := newFixture(t)
	// No credential: 401.
	if resp := f.do("GET", "/api/settings", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key = %d want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Member role carries no settings scopes: 403.
	if resp := f.do("GET", "/api/settings", f.memberKey, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member = %d want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Admin reads fine.
	if resp := f.do("GET", "/api/settings", f.adminKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin = %d want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Health is open (infrastructure probe).
	if resp := f.do("GET", "/api/health", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestSettingsRoundTripAndValidation(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", f.adminKey, map[string]any{
		"provider": "glm", "model": "glm-5", "msg_plane_mode": "pg+nats",
		"msg_plane_nats_url": "nats://cfg:4222",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}
	cfg := f.st.Load()
	if cfg.Provider != "glm" || cfg.Model != "glm-5" {
		t.Fatalf("store = %+v", cfg)
	}
	if cfg.MsgPlaneMode != "pg+nats" || cfg.MsgPlaneNATSURL != "nats://cfg:4222" {
		t.Fatalf("msg plane = %+v", cfg)
	}

	get := f.do("GET", "/api/settings", f.adminKey, nil)
	defer get.Body.Close()
	var out struct {
		Provider     string `json:"provider"`
		MsgPlaneMode string `json:"msg_plane_mode"`
		Available    bool   `json:"msg_plane_available"`
	}
	_ = json.NewDecoder(get.Body).Decode(&out)
	if out.Provider != "glm" || out.MsgPlaneMode != "pg+nats" || !out.Available {
		t.Fatalf("get = %+v", out)
	}

	// Invalid values are rejected with the validator's message.
	bad := f.do("PUT", "/api/settings", f.adminKey, map[string]any{"provider": "bedrock"})
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad provider = %d want 400", bad.StatusCode)
	}
	badMode := f.do("PUT", "/api/settings", f.adminKey, map[string]any{"msg_plane_mode": "kafka"})
	defer badMode.Body.Close()
	if badMode.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mode = %d want 400", badMode.StatusCode)
	}
}

func TestExportImport(t *testing.T) {
	f := newFixture(t)
	if resp := f.do("PUT", "/api/settings", f.adminKey, map[string]any{"model": "exported-model"}); resp.StatusCode != http.StatusOK {
		t.Fatal("seed put failed")
	} else {
		resp.Body.Close()
	}

	// Plain export carries values, never secrets.
	f.st.SetSecret(settings.SecretGLMAPIKey, "woca-secret-value")
	exp := f.do("GET", "/api/settings/export", f.adminKey, nil)
	defer exp.Body.Close()
	var doc exportFile
	if err := json.NewDecoder(exp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Values["model"] != "exported-model" {
		t.Fatalf("export values = %+v", doc.Values)
	}
	if len(doc.Secrets) != 0 {
		t.Fatalf("plain export leaked secrets: %+v", doc.Secrets)
	}

	// include_secrets=true returns the plaintext (admin-only surface).
	sec := f.do("GET", "/api/settings/export?include_secrets=true", f.adminKey, nil)
	defer sec.Body.Close()
	var withSec exportFile
	_ = json.NewDecoder(sec.Body).Decode(&withSec)
	if withSec.Secrets[settings.SecretGLMAPIKey] != "woca-secret-value" {
		t.Fatalf("secrets export = %+v", withSec.Secrets)
	}

	// Import applies a document idempotently; secret keys in values are
	// rejected (they belong in the secrets section).
	imp := f.do("POST", "/api/settings/import", f.adminKey, exportFile{
		Values: map[string]string{"model": "imported-model", "max-tokens": "4096"},
	})
	defer imp.Body.Close()
	if imp.StatusCode != http.StatusOK {
		t.Fatalf("import = %d", imp.StatusCode)
	}
	cfg := f.st.Load()
	if cfg.Model != "imported-model" || cfg.MaxTokens != 4096 {
		t.Fatalf("after import = %+v", cfg)
	}

	badDoc := f.do("POST", "/api/settings/import", f.adminKey, exportFile{
		Values: map[string]string{"glm-api-key": "sneaky"},
	})
	defer badDoc.Body.Close()
	if badDoc.StatusCode != http.StatusBadRequest {
		t.Fatalf("secret-in-values = %d want 400", badDoc.StatusCode)
	}
}

func TestSpaFallback(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/", "/msgplane"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d want 200 (SPA)", path, resp.StatusCode)
		}
	}
	// API paths authenticate before routing: an anonymous miss is 401,
	// an authenticated miss routes to 404.
	resp, err := http.Get(f.srv.URL + "/api/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous api miss = %d want 401", resp.StatusCode)
	}
	authed := f.do("GET", "/api/nope", f.adminKey, nil)
	authed.Body.Close()
	if authed.StatusCode != http.StatusNotFound {
		t.Fatalf("api miss = %d want 404", authed.StatusCode)
	}
}

// The status endpoint reports the distribution inventory: generations plus
// every service ack with liveness and lag flags.
func TestConfigStatusInventory(t *testing.T) {
	f := newFixture(t)
	// A stale service ack (old generation, old timestamp) and a live one.
	now := wocadb.Now()
	if err := f.svc.db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "worker-stale", Role: "worker", Generation: "1/1",
		UpdatedAt: now - 60_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "srv-live", Role: "server", Generation: f.st.Generation(),
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.do("GET", "/api/config/status", f.adminKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var doc struct {
		Generation       string `json:"generation"`
		TenantGeneration string `json:"tenant_generation"`
		Services         []struct {
			ServiceID string `json:"service_id"`
			Live      bool   `json:"live"`
			Lagging   bool   `json:"config_lagging"`
		} `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Generation == "" || doc.TenantGeneration == "" {
		t.Fatalf("generations = %+v", doc)
	}
	byID := map[string]struct {
		ServiceID string `json:"service_id"`
		Live      bool   `json:"live"`
		Lagging   bool   `json:"config_lagging"`
	}{}
	for _, s := range doc.Services {
		byID[s.ServiceID] = s
	}
	if sv, ok := byID["worker-stale"]; !ok || sv.Live || !sv.Lagging {
		t.Fatalf("stale service = %+v ok=%v", sv, ok)
	}
	if sv, ok := byID["srv-live"]; !ok || !sv.Live || sv.Lagging {
		t.Fatalf("live service = %+v ok=%v", sv, ok)
	}
}

// Per-subsystem postgres fields round-trip: checkpoint and memory keep
// their own host/port/db/user rows and password secrets; the consumer
// assembles the DSN.
func TestPerSubsystemPostgresRoundTrip(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", f.adminKey, map[string]any{
		"checkpoint_backend": "postgres",
		"checkpoint_pg_host": "ck-pg", "checkpoint_pg_port": "5433",
		"checkpoint_pg_user": "cku", "checkpoint_pg_password": "ckpw",
		"memory_backend": "postgres",
		"memory_pg_host": "mm-pg", "memory_pg_db": "mmdb",
		"memory_pg_password": "mmpw",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put = %d %s", resp.StatusCode, string(b))
	}
	get := f.do("GET", "/api/settings", f.adminKey, nil)
	defer get.Body.Close()
	var out struct {
		CheckpointBackend string `json:"checkpoint_backend"`
		CheckpointHost    string `json:"checkpoint_pg_host"`
		CheckpointPort    string `json:"checkpoint_pg_port"`
		CheckpointDB      string `json:"checkpoint_pg_db"`
		CheckpointUser    string `json:"checkpoint_pg_user"`
		CheckpointPWSet   bool   `json:"checkpoint_pg_password_set"`
		MemoryBackend     string `json:"memory_backend"`
		MemoryHost        string `json:"memory_pg_host"`
		MemoryPort        string `json:"memory_pg_port"`
		MemoryDB          string `json:"memory_pg_db"`
		MemoryPWSet       bool   `json:"memory_pg_password_set"`
	}
	_ = json.NewDecoder(get.Body).Decode(&out)
	if out.CheckpointBackend != "postgres" || out.CheckpointHost != "ck-pg" ||
		out.CheckpointPort != "5433" || out.CheckpointUser != "cku" || !out.CheckpointPWSet {
		t.Fatalf("checkpoint block = %+v", out)
	}
	// port/db fall back to defaults when not sent
	if out.MemoryBackend != "postgres" || out.MemoryHost != "mm-pg" ||
		out.MemoryDB != "mmdb" || out.MemoryPort != "5432" || !out.MemoryPWSet {
		t.Fatalf("memory block = %+v", out)
	}
	// the assembled DSN the backend builds (credentials URL-escaped)
	cfg := f.st.Load()
	if dsn := settings.CheckpointPostgresDSN(cfg, f.st.GetSecret(settings.SecretCheckpointPGPassword)); dsn != "postgres://cku:ckpw@ck-pg:5433/woca?sslmode=disable" {
		t.Fatalf("assembled checkpoint DSN = %q", dsn)
	}
	if dsn := settings.PostgresDSN("mm-pg", "", "mmdb", "", f.st.GetSecret(settings.SecretMemoryPGPassword)); dsn != "postgres://:mmpw@mm-pg:5432/mmdb?sslmode=disable" {
		t.Fatalf("assembled memory DSN = %q", dsn)
	}
}
