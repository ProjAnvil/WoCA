package server

// Task 14 (EE sandbox plane): session workspace identity. An off-host
// deployment binds each session's workspace to a stable sandbox workspace id
// through the executor proxy's allocate-or-bind API (same tenant + same
// path ⇒ same id), dispatched runs inherit the identity into
// runs.workspace_id, legacy sessions backfill once, and the workspace file
// surfaces resolve session → id → <WorkspaceRoot>/<id> with everything
// outside the volume refused. The personal edition (local tenant, no
// endpoint, no volume root) never touches any of it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// fakeBindingsProxy stands in for the executor proxy's bind endpoint
// (POST /internal/bindings): the workspace id is a deterministic function of
// the source key, so same tenant+path converges on one id and different
// tenants or paths never share. Every request is recorded for assertions.
type fakeBindingsProxy struct {
	urlStr string
	mu     sync.Mutex
	seen   []bindCall
}

type bindCall struct {
	TenantID  string `json:"tenant_id"`
	SourceKey string `json:"source_key"`
}

func newFakeBindingsProxy(t *testing.T) *fakeBindingsProxy {
	t.Helper()
	p := &fakeBindingsProxy{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/bindings", func(w http.ResponseWriter, r *http.Request) {
		var req bindCall
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TenantID == "" || req.SourceKey == "" {
			http.Error(w, "bad bind request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.seen = append(p.seen, req)
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(req.SourceKey))
		writeJSON(w, http.StatusOK, map[string]string{
			"workspace_id": "ws_" + hex.EncodeToString(sum[:8]),
			"executor_id":  "exec-1",
			"state":        "active",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.urlStr = srv.URL
	return p
}

func (p *fakeBindingsProxy) requests() []bindCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bindCall(nil), p.seen...)
}

// multiTenantProvider authenticates by credential so one server serves
// several tenants (the bearer token is the tenant id).
type multiTenantProvider struct{}

func (multiTenantProvider) Authenticate(_ context.Context, credential string) (tenancy.Context, error) {
	if credential == "" {
		credential = "local"
	}
	return tenancy.Context{TenantID: credential}, nil
}

// sandboxPlaneOpts configures the EE off-host posture under test: queue
// dispatch, the run surface opened to remote tenants, a live sandbox
// endpoint and an executor ack so dispatched runs clear the gates.
type sandboxPlaneOpts struct {
	endpoint    string // allocate-or-bind proxy URL ("" = no sandbox endpoint)
	sessionDir  string
	workspaceRt string
}

func newSandboxPlaneServer(t *testing.T, opts sandboxPlaneOpts) (*Server, *runstate.Ledger, *wocadb.DB, *httptest.Server) {
	t.Helper()
	if opts.sessionDir == "" {
		opts.sessionDir = t.TempDir()
	}
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "sandbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "exec-1", Role: "executor", Generation: "g1",
		Capabilities: "sandbox", Addr: "10.0.0.9:8080", UpdatedAt: wocadb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint func() string
	if opts.endpoint != "" {
		ep := opts.endpoint
		endpoint = func() string { return ep }
	}
	s := New(Config{
		Agent: newScriptedAgent(t, "bound"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: opts.sessionDir, WorkDir: t.TempDir(),
		RunLedger: ledger, DispatchToQueue: true, RunsOffHost: true,
		RunStateDB:      db,
		SandboxEnabled:  func() bool { return true },
		SandboxEndpoint: endpoint,
		WorkspaceRoot:   opts.workspaceRt,
		Auth:            multiTenantProvider{},
		// The sandbox plane makes the session and workspace surfaces
		// tenant-safe (workspace browsing resolves inside the executor
		// volume), so the deployment form opens them past the local gate.
		HostGateExemptPrefixes: []string{"/api/sessions", "/api/workspace"},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, ledger, db, srv
}

func postSession(t *testing.T, url, tenant, workspace string) (*http.Response, string) {
	t.Helper()
	body := "{}"
	if workspace != "" {
		body = `{"workspace":` + quoteJSON(workspace) + `}`
	}
	req, err := http.NewRequest(http.MethodPost, url+"/api/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "bearer "+tenant)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out.SessionID
}

func dispatchTenantRun(t *testing.T, url, tenant, sessionID string) (int, string) {
	t.Helper()
	body := "{}"
	if sessionID != "" {
		body = fmt.Sprintf(`{"session_id":%q,"prompt":"queue me"}`, sessionID)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/api/runs", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "bearer "+tenant)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.RunID
}

// TestCreateSessionBindsWorkspaceIdentity: same tenant + same workspace
// path converge on one sandbox workspace id; a different tenant on the same
// path gets a different one (the source key is tenant-scoped).
func TestCreateSessionBindsWorkspaceIdentity(t *testing.T) {
	proxy := newFakeBindingsProxy(t)
	s, _, _, srv := newSandboxPlaneServer(t, sandboxPlaneOpts{endpoint: proxy.urlStr})
	ws := filepath.Join(t.TempDir(), "proj")

	resp, sid1 := postSession(t, srv.URL, "tenant-a", ws)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session = %d", resp.StatusCode)
	}
	_, sid2 := postSession(t, srv.URL, "tenant-a", ws)
	_, sid3 := postSession(t, srv.URL, "tenant-b", ws)

	id1 := s.sessions.workspaceIDOf(sid1)
	id2 := s.sessions.workspaceIDOf(sid2)
	id3 := s.sessions.workspaceIDOf(sid3)
	if id1 == "" || id1 != id2 {
		t.Fatalf("same tenant+path: ids = %q, %q (want equal and non-empty)", id1, id2)
	}
	if id3 == "" || id3 == id1 {
		t.Fatalf("different tenant on same path: id = %q (want distinct from %q)", id3, id1)
	}
	// The proxy saw tenant-scoped source keys over the normalized path.
	reqs := proxy.requests()
	if len(reqs) != 3 {
		t.Fatalf("bind calls = %d, want 3", len(reqs))
	}
	abs, _ := filepath.Abs(ws)
	want := "tenant-a|" + abs
	if reqs[0].SourceKey != want || reqs[0].TenantID != "tenant-a" {
		t.Fatalf("source key = %+v, want %q", reqs[0], want)
	}
}

// TestCreateSessionBindingUnavailableFailsClosed: an unreachable proxy
// fails session creation with 503 — no fallback, and nothing persists (an
// unbound session could never run off-host).
func TestCreateSessionBindingUnavailableFailsClosed(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unreachable", http.StatusBadGateway)
	}))
	url := dead.URL
	dead.Close() // nothing listens anymore

	s, _, _, srv := newSandboxPlaneServer(t, sandboxPlaneOpts{endpoint: url})
	resp, sid := postSession(t, srv.URL, "tenant-a", filepath.Join(t.TempDir(), "p"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create session with dead proxy = %d, want 503", resp.StatusCode)
	}
	if sid != "" || s.sessions.workspaceIDOf(sid) != "" {
		t.Fatalf("session %q must not persist on binding failure", sid)
	}
	if list, err := s.sessions.list(); err != nil || len(list) != 0 {
		t.Fatalf("session index after failed create = %d sessions err=%v", len(list), err)
	}
}

// TestDispatchedRunInheritsWorkspaceID: a run dispatched for a bound session
// carries the session's workspace identity into runs.workspace_id.
func TestDispatchedRunInheritsWorkspaceID(t *testing.T) {
	proxy := newFakeBindingsProxy(t)
	s, _, db, srv := newSandboxPlaneServer(t, sandboxPlaneOpts{endpoint: proxy.urlStr})
	ws := filepath.Join(t.TempDir(), "proj")
	_, sid := postSession(t, srv.URL, "tenant-a", ws)
	want := s.sessions.workspaceIDOf(sid)
	if want == "" {
		t.Fatal("session has no bound workspace id")
	}

	status, runID := dispatchTenantRun(t, srv.URL, "tenant-a", sid)
	if status != http.StatusOK {
		t.Fatalf("POST /api/runs = %d", status)
	}
	var got string
	if err := db.QueryRow(`SELECT workspace_id FROM runs WHERE run_id = ?`, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("runs.workspace_id = %q, want the session binding %q", got, want)
	}
	// The binding already existed: no extra allocate-or-bind call.
	if n := len(proxy.requests()); n != 1 {
		t.Fatalf("bind calls = %d, want 1 (inherit, not re-bind)", n)
	}
}

// TestDispatchedRunBackfillsLegacySession: a session persisted by a
// pre-binding build (index meta without workspace_id) backfills exactly
// once through the same allocate-or-bind API, the run row carries the
// identity, and the session meta records it for the next run.
func TestDispatchedRunBackfillsLegacySession(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "legacy-proj")

	// Server one: the pre-binding posture persists a session normally.
	legacy := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: dir, WorkDir: t.TempDir(),
	})
	lreq := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"workspace":`+quoteJSON(ws)+`}`))
	lresp := httptest.NewRecorder()
	legacy.handleCreateSession(lresp, lreq)
	var created struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(lresp.Body).Decode(&created)
	if created.SessionID == "" {
		t.Fatal("legacy session not created")
	}

	// Server two: the same session directory under the sandbox plane.
	proxy := newFakeBindingsProxy(t)
	s, _, db, srv := newSandboxPlaneServer(t, sandboxPlaneOpts{endpoint: proxy.urlStr, sessionDir: dir})
	if id := s.sessions.workspaceIDOf(created.SessionID); id != "" {
		t.Fatalf("legacy session already bound: %q", id)
	}

	status, runID := dispatchTenantRun(t, srv.URL, "tenant-a", created.SessionID)
	if status != http.StatusOK {
		t.Fatalf("POST /api/runs = %d", status)
	}
	var got string
	if err := db.QueryRow(`SELECT workspace_id FROM runs WHERE run_id = ?`, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("backfilled run has no workspace id")
	}
	if id := s.sessions.workspaceIDOf(created.SessionID); id != got {
		t.Fatalf("session meta after backfill = %q, want %q (write-back)", id, got)
	}
	// The backfill reused the session's recorded workspace as the source
	// (not the request fallback), so a re-dispatch inherits without a call.
	if reqs := proxy.requests(); len(reqs) != 1 || !strings.HasSuffix(reqs[0].SourceKey, ws) {
		t.Fatalf("backfill source keys = %+v", reqs)
	}
}

// TestWorkspaceVolumeResolution: with a WorkspaceRoot the workspace surfaces
// resolve session → workspace id → <root>/<id>, refuse requests that name
// no bound session (never falling back to host paths), and refuse a
// corrupted identity that would escape the volume (/etc-class targets).
// The personal posture (no root) is unchanged: ?workspace= resolves as
// before.
func TestWorkspaceVolumeResolution(t *testing.T) {
	root := t.TempDir()
	proxy := newFakeBindingsProxy(t)
	s, _, _, srv := newSandboxPlaneServer(t, sandboxPlaneOpts{endpoint: proxy.urlStr, workspaceRt: root})
	ws := filepath.Join(t.TempDir(), "proj")
	_, sid := postSession(t, srv.URL, "tenant-a", ws)
	wsID := s.sessions.workspaceIDOf(sid)
	if wsID == "" {
		t.Fatal("session has no bound workspace id")
	}
	// The executor volume holds the workspace's directory.
	volDir := filepath.Join(root, wsID)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "notes.md"), []byte("hello volume"), 0o644); err != nil {
		t.Fatal(err)
	}

	get := func(url string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "bearer tenant-a")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Bound session resolves inside the volume.
	resp := get(srv.URL + "/api/workspace?session=" + sid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workspace tree = %d", resp.StatusCode)
	}
	var tree struct {
		Children []struct {
			Name string `json:"name"`
		} `json:"children"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tree)
	resp.Body.Close()
	found := false
	for _, c := range tree.Children {
		if c.Name == "notes.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("volume file not listed: %d children", len(tree.Children))
	}

	// File preview resolves through the same chain.
	resp = get(srv.URL + "/api/workspace/file?session=" + sid + "&path=notes.md")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volume file preview = %d", resp.StatusCode)
	}
	var file struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&file)
	if file.Content != "hello volume" {
		t.Fatalf("volume file content = %q", file.Content)
	}

	// No session (and a host path like /etc via ?workspace=) is refused —
	// never a host-path fallback.
	resp = get(srv.URL + "/api/workspace")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("workspace without session = %d, want 403", resp.StatusCode)
	}
	resp = get(srv.URL + "/api/workspace?workspace=/etc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("host path in EE = %d, want 403", resp.StatusCode)
	}

	// A corrupted identity that escapes the volume root is refused too.
	s.sessions.setWorkspaceID(sid, "../../etc")
	resp = get(srv.URL + "/api/workspace?session=" + sid)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("escaping identity = %d, want 403", resp.StatusCode)
	}
}

// TestPersonalEditionWorkspaceUnchanged: without a WorkspaceRoot the
// workspace surface keeps the personal resolution (?workspace=, else the
// workdir) — the regression net for the resolution-chain change.
func TestPersonalEditionWorkspaceUnchanged(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: dir, WorkDir: dir,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/workspace?workspace=" + dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("personal workspace = %d", resp.StatusCode)
	}
	var tree struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tree)
	if tree.Name == "" {
		t.Fatal("personal workspace tree missing")
	}
}

// TestPersonalSessionsNeverBind: a local tenant never allocates a binding,
// even when the sandbox endpoint is wired — the personal edition has no
// tenant plane to scope a source key with.
func TestPersonalSessionsNeverBind(t *testing.T) {
	proxy := newFakeBindingsProxy(t)
	personal := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: openRunLedger(t), DispatchToQueue: true, RunsOffHost: true,
		SandboxEnabled:  func() bool { return true },
		SandboxEndpoint: func() string { return proxy.urlStr },
		Auth:            tenantProvider{tenant: "local"},
	})
	psrv := httptest.NewServer(personal.Handler())
	defer psrv.Close()

	resp, sid := postSession(t, psrv.URL, "local", filepath.Join(t.TempDir(), "p"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local session create = %d", resp.StatusCode)
	}
	if id := personal.sessions.workspaceIDOf(sid); id != "" {
		t.Fatalf("local session bound: %q", id)
	}
	if n := len(proxy.requests()); n != 0 {
		t.Fatalf("local tenant triggered %d bind calls", n)
	}
}
