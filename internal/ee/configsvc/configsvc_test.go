package configsvc

// Tests cover the management-plane contract: the console gate (open by
// default, session-cookie login when credentials are configured), the
// settings GET/PUT round trip with validation, export/import idempotence,
// the SPA fallback, the distribution-status inventory, the org identity
// domain riding the bridge config package, and the per-subsystem
// postgres fields.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

type fixture struct {
	svc     *Service
	srv     *httptest.Server
	st      *settings.Store
	db      *wocadb.DB
	tenants *tenant.Store
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
	svc, err := New(st, ts, db, zerolog.Nop())
	if err == nil {
		svc.AttachPublisher(&fakePublisher{})
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	// The fixture runs in the open posture (no WOCA_CONSOLE_USER set):
	// every request carries the console-admin tenancy.
	return &fixture{svc: svc, srv: srv, st: st, db: db, tenants: ts}
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
	// Unconfigured credentials: the console is open — no login, no keys.
	if resp := f.do("GET", "/api/settings", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("open mode = %d want 200", resp.StatusCode)
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
	resp := f.do("PUT", "/api/settings", "", map[string]any{
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

	get := f.do("GET", "/api/settings", "", nil)
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
	bad := f.do("PUT", "/api/settings", "", map[string]any{"provider": "bedrock"})
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad provider = %d want 400", bad.StatusCode)
	}
	badMode := f.do("PUT", "/api/settings", "", map[string]any{"msg_plane_mode": "kafka"})
	defer badMode.Body.Close()
	if badMode.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mode = %d want 400", badMode.StatusCode)
	}
}

func TestExportImport(t *testing.T) {
	f := newFixture(t)
	if resp := f.do("PUT", "/api/settings", "", map[string]any{"model": "exported-model"}); resp.StatusCode != http.StatusOK {
		t.Fatal("seed put failed")
	} else {
		resp.Body.Close()
	}

	// Plain export carries values, never secrets.
	f.st.SetSecret(settings.SecretGLMAPIKey, "woca-secret-value")
	exp := f.do("GET", "/api/settings/export", "", nil)
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
	sec := f.do("GET", "/api/settings/export?include_secrets=true", "", nil)
	defer sec.Body.Close()
	var withSec exportFile
	_ = json.NewDecoder(sec.Body).Decode(&withSec)
	if withSec.Secrets[settings.SecretGLMAPIKey] != "woca-secret-value" {
		t.Fatalf("secrets export = %+v", withSec.Secrets)
	}

	// Import applies a document idempotently; secret keys in values are
	// rejected (they belong in the secrets section).
	imp := f.do("POST", "/api/settings/import", "", exportFile{
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

	badDoc := f.do("POST", "/api/settings/import", "", exportFile{
		Values: map[string]string{"glm-api-key": "sneaky"},
	})
	defer badDoc.Body.Close()
	if badDoc.StatusCode != http.StatusBadRequest {
		t.Fatalf("secret-in-values = %d want 400", badDoc.StatusCode)
	}
}

func TestSpaFallback(t *testing.T) {
	f := newFixture(t)
	// Fresh clones ship only spa/.gitkeep: the handler then serves the 503
	// placeholder (see configsvc.go), so the SPA assertions follow whichever
	// posture this checkout is in.
	_, statErr := fs.Stat(spaFS, "spa/index.html")
	spaBuilt := statErr == nil
	wantSpa := http.StatusOK
	if !spaBuilt {
		wantSpa = http.StatusServiceUnavailable
	}
	for _, path := range []string{"/", "/msgplane"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != wantSpa {
			t.Fatalf("%s = %d want %d (spaBuilt=%v)", path, resp.StatusCode, wantSpa, spaBuilt)
		}
	}
	// API paths authenticate before routing: an anonymous miss is 401,
	// an authenticated miss routes to 404.
	resp, err := http.Get(f.srv.URL + "/api/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Open mode: an unknown API path is a plain 404 (no auth layer to
	// reject it first).
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("anonymous api miss = %d want 404", resp.StatusCode)
	}
	authed := f.do("GET", "/api/nope", "", nil)
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
	resp := f.do("GET", "/api/config/status", "", nil)
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
	resp := f.do("PUT", "/api/settings", "", map[string]any{
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
	get := f.do("GET", "/api/settings", "", nil)
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

// fetchPayload reads the bridge config package directly from the
// service (the machine-channel HTTP path needs an enrolled access token;
// the payload content is identical).
func fetchPayload(t *testing.T, svc *Service) (map[string]string, string) {
	t.Helper()
	version, kv, err := svc.bridgeConfig(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	return kv, version
}

// TestConsoleLoginFlow: with WOCA_CONSOLE_USER/PASSWORD set the console
// gates every /api route behind a session cookie; wrong credentials
// reject with the enumerated code; logout invalidates.
func TestConsoleLoginFlow(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	f := newFixture(t)

	// No session: 401 with the code the frontend switches on.
	resp := f.do("GET", "/api/settings", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %d want 401", resp.StatusCode)
	}

	// Wrong password.
	lr := f.do("POST", "/api/console/login", "", map[string]string{"username": "admin", "password": "wrong"})
	lr.Body.Close()
	if lr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d want 401", lr.StatusCode)
	}

	// Right credentials: session cookie; the API surface opens.
	lr = f.do("POST", "/api/console/login", "", map[string]string{"username": "admin", "password": "s3cret"})
	lr.Body.Close()
	if lr.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", lr.StatusCode)
	}
	var token string
	for _, c := range lr.Cookies() {
		if c.Name == "woca_console_session" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie")
	}
	get := f.doRawCookie("GET", "/api/settings", token)
	get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("with session = %d want 200", get.StatusCode)
	}

	// Logout invalidates the session.
	out := f.doRawCookie("POST", "/api/console/logout", token)
	out.Body.Close()
	after := f.doRawCookie("GET", "/api/settings", token)
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout = %d want 401", after.StatusCode)
	}
}

// doRawCookie issues a request carrying a raw session cookie.
func (f *fixture) doRawCookie(method, path, cookie string) *http.Response {
	req, _ := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(nil))
	req.AddCookie(&http.Cookie{Name: "woca_console_session", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// TestPostgresOnlyBackends: the enterprise config plane rejects
// non-postgres-grade persistence for checkpoint and memory.
func TestPostgresOnlyBackends(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{"checkpoint_backend", "memory_backend"} {
		resp := f.do("PUT", "/api/settings", "", map[string]any{key: "sqlite"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s=sqlite = %d want 400", key, resp.StatusCode)
		}
	}
	resp := f.do("PUT", "/api/settings", "", map[string]any{"checkpoint_backend": "postgres", "memory_backend": "postgres"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("postgres backends = %d want 200", resp.StatusCode)
	}
}

// TestGWModelCatalog: the machine-channel catalog — the list carries no
// credentials; verify authorizes the preferred model and returns the
// preset including its API key (machine channel only).
func TestGWModelCatalog(t *testing.T) {
	f := newFixture(t)

	// Author a preset (console API) with a key.
	resp := f.do("POST", "/api/models", "", map[string]any{
		"name": "anthro", "provider": "anthropic", "model": "claude-x", "api_key": "sk-secret",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save preset = %d", resp.StatusCode)
	}

	// Enroll a bridge to get a machine access token.
	gw, err := entrygw.NewStore(f.db)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := gw.IssueEnrollment(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridgeID, err := gw.ConsumeEnrollment(t.Context(), sha256hex(tok), []byte("pub"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	access, _, err := gw.IssueAccess(t.Context(), bridgeID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path string, body any) (*http.Response, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, raw
	}

	resp, raw := do("GET", "/gw/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, raw)
	}
	if bytes.Contains(raw, []byte("sk-secret")) {
		t.Fatalf("list leaks the credential: %s", raw)
	}
	if !bytes.Contains(raw, []byte("claude-x")) {
		t.Fatalf("preset missing: %s", raw)
	}

	// Anonymous machine-channel calls bounce.
	respAnon, _ := http.Get(f.srv.URL + "/gw/models")
	respAnon.Body.Close()
	if respAnon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous gw/models = %d want 401", respAnon.StatusCode)
	}

	// Verify by provider+model: authorized, key included.
	resp, raw = do("POST", "/gw/models/verify", map[string]string{"provider": "anthropic", "model": "claude-x"})
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte("sk-secret")) {
		t.Fatalf("verify = %d %s", resp.StatusCode, raw)
	}
	// Unknown model: refused, uniform.
	resp, _ = do("POST", "/gw/models/verify", map[string]string{"provider": "anthropic", "model": "nope"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model = %d want 404", resp.StatusCode)
	}
}

func sha256hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// TestManagedSkillsMCP: operator CRUD round trip and the bridge package
// carrying the delivered set.
func TestManagedSkillsMCP(t *testing.T) {
	f := newFixture(t)

	resp := f.do("PUT", "/api/skills", "", map[string]string{
		"name": "pdf-report", "description": "write pdf reports", "body": "# pdf\n\ndo the thing",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save skill = %d", resp.StatusCode)
	}
	resp = f.do("PUT", "/api/mcp", "", map[string]string{"name": "fetch", "spec": "uvx mcp-server-fetch"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save mcp = %d", resp.StatusCode)
	}
	// Bad spec grammar refused.
	resp = f.do("PUT", "/api/mcp", "", map[string]string{"name": "bad", "spec": "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad spec = %d want 400", resp.StatusCode)
	}

	payload, _ := fetchPayload(t, f.svc)
	if !strings.Contains(payload["managed-skills"], "pdf-report") {
		t.Fatalf("skills missing from package: %+v", payload["managed-skills"])
	}
	if !strings.Contains(payload["managed-mcp"], "fetch=uvx mcp-server-fetch") {
		t.Fatalf("mcp missing from package: %+v", payload["managed-mcp"])
	}

	// Delete propagates as an empty set (the delivered set is the full
	// inventory — removals ride the package).
	resp = f.do("DELETE", "/api/skills/pdf-report", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete skill = %d", resp.StatusCode)
	}
	payload, _ = fetchPayload(t, f.svc)
	if strings.Contains(payload["managed-skills"], "pdf-report") {
		t.Fatalf("deleted skill still in package: %+v", payload["managed-skills"])
	}
}

// TestStorageMemoryRidePackage: the storage domain the operator configures
// (split postgres coordinates, remote ladyM endpoint, enterprise plugin
// set) rides the bridge package — without the wire a "backend: postgres /
// engine: remote" delivery leaves bridges with nowhere to connect.
func TestStorageMemoryRidePackage(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", "", map[string]any{
		"checkpoint_backend": "postgres",
		"checkpoint_pg_host": "ck-pg", "checkpoint_pg_user": "cku", "checkpoint_pg_password": "ckpw",
		"memory_backend": "postgres",
		"memory_pg_host": "mm-pg", "memory_pg_password": "mmpw",
		"memory_engine":  "remote",
		"ladym_endpoint": "https://ladym.corp.example", "ladym_api_username": "bridge", "ladym_api_password": "lmpw",
		"builtin_plugins": []string{"browser"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}

	payload, _ := fetchPayload(t, f.svc)
	for _, k := range []string{
		"checkpoint-pg-host", "checkpoint-pg-user", "checkpoint-pg-password",
		"memory-pg-host", "memory-pg-password",
		"ladym-endpoint", "ladym-api-username", "ladym-api-password",
		"builtin-plugins",
	} {
		if payload[k] == "" {
			t.Errorf("package key %q missing/empty (payload: %+v)", k, payload)
		}
	}
	if payload["checkpoint-pg-host"] != "ck-pg" || payload["checkpoint-pg-password"] != "ckpw" {
		t.Fatalf("checkpoint delivery = %q / %q", payload["checkpoint-pg-host"], payload["checkpoint-pg-password"])
	}
	if payload["builtin-plugins"] != `["browser"]` {
		t.Fatalf("plugin set = %q", payload["builtin-plugins"])
	}
}

// TestSandboxBackendsMigration: New provisions the sandbox_backends table
// (design §4.2) — the management plane's sandbox backend inventory.
func TestSandboxBackendsMigration(t *testing.T) {
	f := newFixture(t)
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM sandbox_backends`).Scan(&n); err != nil {
		t.Fatalf("sandbox_backends table missing after New: %v", err)
	}
	if n != 0 {
		t.Fatalf("fresh table has %d rows, want 0", n)
	}
}

// fakePublisher records deliveries for assertions.
type fakePublisher struct {
	published []string
}

func (f *fakePublisher) Publish(_ context.Context, _, _, content string) error {
	f.published = append(f.published, content)
	return nil
}

// TestLangfuseConsoleRoundTrip: the console owns the Langfuse plugin —
// keys persist, the secret lands encrypted, and GET reports *_set.
func TestLangfuseConsoleRoundTrip(t *testing.T) {
	f := newFixture(t)
	resp := f.do("PUT", "/api/settings", "", map[string]any{
		"langfuse_enabled": true, "langfuse_base_url": "https://lf.corp.example",
		"langfuse_public_key": "pk-lf", "langfuse_secret_key": "sk-lf",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}
	cfg := f.st.Load()
	if !cfg.LangfuseEnabled || cfg.LangfuseBaseURL != "https://lf.corp.example" || cfg.LangfusePublicKey != "pk-lf" {
		t.Fatalf("store = %+v", cfg)
	}
	if got := f.st.GetSecret(settings.SecretLangfuseSecretKey); got != "sk-lf" {
		t.Fatalf("secret = %q", got)
	}

	get := f.do("GET", "/api/settings", "", nil)
	defer get.Body.Close()
	var out struct {
		Enabled   bool   `json:"langfuse_enabled"`
		BaseURL   string `json:"langfuse_base_url"`
		PublicKey string `json:"langfuse_public_key"`
		SecretSet bool   `json:"langfuse_secret_key_set"`
	}
	_ = json.NewDecoder(get.Body).Decode(&out)
	if !out.Enabled || out.BaseURL != "https://lf.corp.example" || out.PublicKey != "pk-lf" || !out.SecretSet {
		t.Fatalf("get = %+v", out)
	}

	// The bridge package carries the plugin when enabled (secret via
	// ManagedSecretKeys).
	_, kv, err := f.svc.bridgeConfig(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if kv["langfuse-enabled"] != "true" || kv["langfuse-base-url"] != "https://lf.corp.example" {
		t.Fatalf("bridgeConfig langfuse = %v", kv)
	}
}
