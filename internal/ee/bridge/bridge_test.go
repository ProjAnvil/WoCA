package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/wocadb"
)

// fakeCore stands in for the private core's identity service: it issues
// sessions at login and answers introspections from its own table,
// with injectable reachability.
type fakeCore struct {
	mu           sync.Mutex
	reachable    bool
	nextExp      int64
	revoked      map[string]bool // token → revoked
	issued       []CoreSession
	introspected []string
	logoutCount  int
}

func newFakeCore() *fakeCore {
	return &fakeCore{reachable: true, revoked: map[string]bool{}, nextExp: time.Now().Add(sessionTTL).UnixMilli()}
}

func (f *fakeCore) CoreLogin(_ context.Context, apiKey string) (CoreSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reachable {
		return CoreSession{}, ErrCoreUnreachable
	}
	if strings.HasPrefix(apiKey, "woca_revoked") {
		return CoreSession{}, errors.New("core: 401 code=revoked")
	}
	if !strings.HasPrefix(apiKey, "woca_") {
		return CoreSession{}, errors.New("core: 401")
	}
	role := authz.RoleMember
	if strings.Contains(apiKey, "owner") {
		role = authz.RoleAdmin
	}
	sess := CoreSession{
		Token:    "wocs_" + apiKey[len("woca_"):len("woca_")+12],
		TenantID: "local", UserID: "u_" + apiKey[len("woca_"):len("woca_")+6],
		Remark: "user-" + apiKey[len("woca_"):len("woca_")+4], Role: role,
		ExpiresAt: f.nextExp,
	}
	f.issued = append(f.issued, sess)
	return sess, nil
}

func (f *fakeCore) CoreIntrospect(_ context.Context, token string) (CoreSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.introspected = append(f.introspected, token)
	if !f.reachable {
		return CoreSession{}, ErrCoreUnreachable
	}
	for _, s := range f.issued {
		if s.Token != token {
			continue
		}
		if f.revoked[token] {
			return CoreSession{Revoked: true}, nil
		}
		return s, nil
	}
	return CoreSession{}, nil // active=false, no code: plain invalid
}

func (f *fakeCore) CoreLogout(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reachable {
		return ErrCoreUnreachable
	}
	f.logoutCount++
	f.revoked[token] = true
	return nil
}

func (f *fakeCore) setReachable(v bool)  { f.mu.Lock(); f.reachable = v; f.mu.Unlock() }
func (f *fakeCore) revokeToken(t string) { f.mu.Lock(); f.revoked[t] = true; f.mu.Unlock() }

// newTestAuth builds the bridge identity surface on a temp database.
func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := NewAuth(db)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// newBridgeServer mounts the full bridge surface on a test server with
// the given core behind the identity channel.
func newBridgeServer(t *testing.T, core *fakeCore) (*Auth, *httptest.Server) {
	t.Helper()
	a := newTestAuth(t)
	if core != nil {
		a.SetIntrospector(core)
	}
	srv := server.New(server.Config{
		Auth:    a,
		Static:  fstest.MapFS{},
		Logger:  zerolog.Nop(),
		WorkDir: t.TempDir(),
		Extras:  a.RegisterRoutes,
		// The bridge's declared bootstrap exchange (mirrors the entry
		// point): login is anonymous, cookie sessions ride the provider's
		// CookieCredential declaration.
		AnonymousAPIPaths:      []string{"/api/auth/login"},
		HostGateExemptPrefixes: []string{"/api/auth/login"},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return a, ts
}

func postJSON(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getBearer(url, token string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

// TestLoginForwardsToCore: the login exchange forwards over the machine
// channel, mints the cookie + token, and the session authenticates.
func TestLoginForwardsToCore(t *testing.T) {
	core := newFakeCore()
	a, ts := newBridgeServer(t, core)

	// Unauthenticated API access is rejected (R2).
	if resp, _ := http.Get(ts.URL + "/api/sessions"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d", resp.StatusCode)
	}

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_alice99_secret"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	var out struct {
		Token  string `json:"token"`
		Role   string `json:"role"`
		Remark string `json:"remark"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasPrefix(out.Token, "wocs_") || out.Role != authz.RoleMember || out.Remark == "" {
		t.Fatalf("login body = %+v", out)
	}
	loginCookie := findCookie(resp, SessionCookie)
	if loginCookie == nil || !loginCookie.HttpOnly || loginCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie missing: %v", resp.Cookies())
	}

	// The session authenticates as Bearer and as cookie.
	if br, _ := getBearer(ts.URL+"/api/sessions", out.Token); br.StatusCode != http.StatusOK {
		t.Fatalf("bearer session = %d", br.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions", nil)
	req.AddCookie(loginCookie)
	cr, err := http.DefaultClient.Do(req)
	if err != nil || cr.StatusCode != http.StatusOK {
		t.Fatalf("cookie session = %v %v", cr, err)
	}

	// Session tokens cannot be re-presented as login credentials.
	resp2 := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": out.Token})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("session-as-login = %d want 400", resp2.StatusCode)
	}
	_ = a
}

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestCoreUnreachableFailClosedNewLogins: with the core down, NEW logins
// are refused (fail-closed) and unknown tokens do not authenticate.
func TestCoreUnreachableFailClosedNewLogins(t *testing.T) {
	core := newFakeCore()
	_, ts := newBridgeServer(t, core)
	core.setReachable(false)

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_alice99_secret"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login with core down = %d want 503", resp.StatusCode)
	}
	// Unknown session tokens fail closed too (no mirror row to stand on).
	if resp, _ = getBearer(ts.URL+"/api/sessions", "wocs_nosuchtoken0000000000"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token with core down = %d want 401", resp.StatusCode)
	}
}

// TestRevocationAndGrace: a core-side revocation kills the mirrored
// session with the enumerated code (on the next re-validation); a core
// OUTAGE leaves the established session alive (monotone degradation).
func TestRevocationAndGrace(t *testing.T) {
	oldGap := recheckGap
	recheckGap = 10 * time.Millisecond
	t.Cleanup(func() { recheckGap = oldGap })
	core := newFakeCore()
	_, ts := newBridgeServer(t, core)

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_carol77_secret"})
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	// Force the re-validation window to elapse, then revoke at the core.
	time.Sleep(recheckGap + 30*time.Millisecond)
	core.revokeToken(out.Token)
	r, _ := getBearer(ts.URL+"/api/sessions", out.Token)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized || body.Code != "S-B-0002" {
		t.Fatalf("revoked session = %d code=%q", r.StatusCode, body.Code)
	}
}

// TestOutageGrace keeps an established session alive without the core.
func TestOutageGrace(t *testing.T) {
	oldGap := recheckGap
	recheckGap = 10 * time.Millisecond
	t.Cleanup(func() { recheckGap = oldGap })
	core := newFakeCore()
	_, ts := newBridgeServer(t, core)

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_dave55_secret"})
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	time.Sleep(recheckGap + 30*time.Millisecond)
	core.setReachable(false)
	r, _ := getBearer(ts.URL+"/api/sessions", out.Token)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("established session during outage = %d want 200 (grace)", r.StatusCode)
	}
}

// TestTunnelAssertion: requests carrying the edge's identity assertion
// authenticate as that identity — the scenario-four path (§2.2). The
// assertion cannot arrive as a client header; it rides the tunnel
// payload's context.
func TestTunnelAssertion(t *testing.T) {
	// The tunnel calls the handler IN-PROCESS (context travels only
	// in-process — an HTTP hop would drop it), exactly like
	// TunnelClient.execute does.
	a := newTestAuth(t)
	srv := server.New(server.Config{
		Auth:                   a,
		Static:                 fstest.MapFS{},
		Logger:                 zerolog.Nop(),
		WorkDir:                t.TempDir(),
		Extras:                 a.RegisterRoutes,
		AnonymousAPIPaths:      []string{"/api/auth/login"},
		HostGateExemptPrefixes: []string{"/api/auth/login"},
	})
	handler := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req = req.WithContext(WithActor(req.Context(), ActorAssertion{
		TenantID: "local", UserID: "u_edge1", Remark: "edge-user", Role: authz.RoleAdmin,
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var me map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&me)
	if rec.Code != http.StatusOK || me["role"] != authz.RoleAdmin {
		t.Fatalf("asserted request = %d %v", rec.Code, me)
	}
}

func TestCheckBindFailClosed(t *testing.T) {
	for _, tc := range []struct {
		addr, auth string
		ok         bool
	}{
		{"127.0.0.1:3484", "", true},
		{"localhost:3484", "local", true},
		{"127.0.0.1:3484", "apikey", true},
		{"0.0.0.0:8080", "", false},
		{":8080", "local", false},
		{"192.168.1.10:8080", "", false},
		{"0.0.0.0:8080", "apikey", true},
	} {
		err := CheckBind(tc.addr, tc.auth)
		if tc.ok && err != nil {
			t.Errorf("CheckBind(%q,%q) = %v, want nil", tc.addr, tc.auth, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("CheckBind(%q,%q) = nil, want refusal", tc.addr, tc.auth)
		}
	}
}

// TestAdminSurfaceIsReadOnly: the owner surface resolves remarks, lists
// live sessions, and serves the audit trail — and the key-management
// routes are gone entirely (v1.8: no user credentials on the bridge).
func TestAdminSurfaceIsReadOnly(t *testing.T) {
	core := newFakeCore()
	a, ts := newBridgeServer(t, core)

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_owner42_secret"})
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	// /api/auth/me resolves the remembered remark and the admin role.
	me := authedGet(t, ts.URL+"/api/auth/me", out.Token)
	if me["role"] != authz.RoleAdmin {
		t.Fatalf("me = %v", me)
	}

	// The key-management routes do not exist — probed on the bare admin
	// mux so the app-level SPA fallback cannot mask it.
	adminMux := http.NewServeMux()
	a.RegisterAdminRoutes(adminMux)
	probe := httptest.NewServer(adminMux)
	defer probe.Close()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, probe.URL+"/api/keys", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /api/keys = %d, want 404", method, resp.StatusCode)
		}
	}

	// The session inventory shows the mirrored session by remark.
	sessions := authedList(t, ts.URL+"/api/sessions/admin", out.Token)
	found := false
	for _, s := range sessions {
		if strings.Contains(fmtString(s["remark"]), "user-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("session remark missing: %v", sessions)
	}

	// The audit trail names people.
	events := authedList(t, ts.URL+"/api/audit", out.Token)
	var haveLogin bool
	for _, ev := range events {
		if ev["action"] == "login" && strings.Contains(fmtString(ev["actor"]), "user-") {
			haveLogin = true
		}
	}
	if !haveLogin {
		t.Fatalf("no named login events: %v", events)
	}
}

func fmtString(v any) string { return strings.TrimSpace(strings.Trim(fmtString2(v), `"`)) }
func fmtString2(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func authedGet(t *testing.T, url, token string) map[string]any {
	t.Helper()
	resp, err := getBearer(url, token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func authedList(t *testing.T, url, token string) []map[string]any {
	t.Helper()
	resp, err := getBearer(url, token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLogoutRevokesAtCore: the bridge logout must revoke the session at
// the core (the authority) — otherwise the token would re-validate
// online and resurrect. Locally the mirror dies immediately.
func TestLogoutRevokesAtCore(t *testing.T) {
	core := newFakeCore()
	_, ts := newBridgeServer(t, core)

	resp := postJSON(t, ts.URL+"/api/auth/login", "", map[string]string{"api_key": "woca_frank11_secret"})
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	core.mu.Lock()
	logoutsBefore := core.logoutCount
	core.mu.Unlock()

	resp2 := postJSON(t, ts.URL+"/api/auth/logout", out.Token, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", resp2.StatusCode)
	}
	core.mu.Lock()
	logouts, revoked := core.logoutCount, core.revoked[out.Token]
	core.mu.Unlock()
	if logouts != logoutsBefore+1 || !revoked {
		t.Fatalf("core logout not performed: logouts=%d revoked=%v", logouts, revoked)
	}
	// The local mirror died: the token no longer authenticates (and the
	// fake core now reports it revoked on revalidation).
	r, _ := getBearer(ts.URL+"/api/sessions", out.Token)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized || body.Code != "S-B-0002" {
		t.Fatalf("token after logout = %d code=%q", r.StatusCode, body.Code)
	}
}

// TestGatewayClientLoginWireShape: the real GatewayClient must surface
// the core's enumerated failure code from the actual 401 body (not an
// error-string convention the unit fake invented).
func TestGatewayClientLoginWireShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","code":"revoked"}`))
	}))
	defer srv.Close()
	g := NewGatewayClient(GatewayConfig{URL: srv.URL}, zerolog.Nop())
	g.SetAccessTokenForTest("tok")

	_, err := g.AuthLogin(t.Context(), "woca_ab12cd34_"+strings.Repeat("0", 48))
	if !errors.Is(err, ErrCoreLoginRevoked) {
		t.Fatalf("revoked login via real client = %v", err)
	}
}

// TestGatewayClientModelCatalogWire: fetch and verify against the real
// HTTP shapes the config plane serves.
func TestGatewayClientModelCatalogWire(t *testing.T) {
	var verified bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gw/models":
			_, _ = w.Write([]byte(`[{"id":"p1","name":"a","provider":"anthropic","model":"claude-x","base_url":""}]`))
		case "/gw/models/verify":
			verified = true
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "nope") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"unknown model"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"p1","provider":"anthropic","model":"claude-x","api_key":"sk-wire"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := NewGatewayClient(GatewayConfig{URL: srv.URL}, zerolog.Nop())
	g.SetAccessTokenForTest("tok")

	presets, err := g.FetchModels(t.Context())
	if err != nil || len(presets) != 1 || presets[0].Model != "claude-x" || presets[0].APIKey != "" {
		t.Fatalf("fetch = %+v err=%v", presets, err)
	}
	p, err := g.VerifyModel(t.Context(), "p1", "", "")
	if err != nil || p.APIKey != "sk-wire" || !verified {
		t.Fatalf("verify = %+v err=%v verified=%v", p, err, verified)
	}
	if _, err := g.VerifyModel(t.Context(), "", "anthropic", "nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown verify = %v", err)
	}
}

// TestMaterializeManagedSets: delivered skills and MCP specs land under
// the managed root (the lowest merge layer), and an empty delivery
// clears them (the delivered set is the full inventory).
func TestMaterializeManagedSets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WOCA_HOME", root)

	raw := `[{"name":"pdf","description":"pdf stuff","body":"# pdf\n\nbody"}]`
	if err := materializeManagedSkills(raw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "managed", "skills", "pdf", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: pdf") || !strings.Contains(string(data), "body") {
		t.Fatalf("SKILL.md = %q", string(data))
	}

	if err := materializeManagedMCP(`["fetch=uvx mcp-server-fetch","docs=https://docs.example.com/mcp"]`); err != nil {
		t.Fatal(err)
	}
	mcpData, err := os.ReadFile(filepath.Join(root, "managed", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(mcpData, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Fatalf("mcp.json = %s", string(mcpData))
	}

	// Empty delivery clears; a malformed delivery keeps the previous set
	// (§3.2 — a broken package never wipes local state).
	if err := materializeManagedSkills("[]"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "managed", "skills", "pdf")); !os.IsNotExist(err) {
		t.Fatal("cleared delivery did not remove the skill")
	}
	if err := materializeManagedMCP("not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "managed", "mcp.json")); err != nil {
		t.Fatal("malformed delivery wiped the previous mcp set")
	}
}
