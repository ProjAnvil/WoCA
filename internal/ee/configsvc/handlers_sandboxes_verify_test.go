package configsvc

// Verify probe tests (design §4.3): the full-chain liveness probe runs
// the full contract path through the backend's routing proxy — a fake
// executor-proxy over a real contract v2 backend (fakesandbox) — binds a
// stable verify workspace per backend, persists the last_probe_* columns,
// and never echoes the stored key.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/platform/httpx"
)

// verifyProxy is a fake executor-proxy (spec §3.2) in front of a real
// contract backend: POST /internal/bindings is allocate-or-bind over an
// in-memory source-key map (a fresh ws-verify-<n> per key, the same id
// for every later call on that key), and every /sandbox/* request
// forwards to the upstream only when it carries X-Woca-Workspace — the
// real proxy's route-on-header-alone rule, so a probe that skips the
// binding step dies at create exactly like it would in production.
type verifyProxy struct {
	mu       sync.Mutex
	next     int
	wsBySrc  map[string]string
	binds    []verifyBindCall
	creates  []verifyCreateCall
	upstream *url.URL
	rp       *httputil.ReverseProxy
}

type verifyBindCall struct {
	Tenant, Source string
}

type verifyCreateCall struct {
	BodyWorkspace, HeaderWorkspace string
}

func newVerifyProxy(t *testing.T, upstream *httptest.Server) *verifyProxy {
	t.Helper()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &verifyProxy{
		wsBySrc:  map[string]string{},
		upstream: u,
		rp:       httputil.NewSingleHostReverseProxy(u),
	}
}

func (p *verifyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/bindings" {
		var req struct {
			TenantID  string `json:"tenant_id"`
			SourceKey string `json:"source_key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil ||
			req.TenantID == "" || req.SourceKey == "" {
			httpx.WriteError(w, http.StatusBadRequest, "bindings: tenant_id and source_key are required")
			return
		}
		p.mu.Lock()
		p.binds = append(p.binds, verifyBindCall{Tenant: req.TenantID, Source: req.SourceKey})
		ws, ok := p.wsBySrc[req.SourceKey]
		if !ok {
			p.next++
			ws = "ws-verify-" + strconv.Itoa(p.next)
			p.wsBySrc[req.SourceKey] = ws
		}
		p.mu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, map[string]string{
			"workspace_id": ws, "executor_id": "exec-test", "state": "active",
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/sandbox/") {
		if r.Header.Get("X-Woca-Workspace") == "" {
			httpx.WriteError(w, http.StatusBadRequest, "missing X-Woca-Workspace header: route every sandbox request with it")
			return
		}
		if r.URL.Path == "/sandbox/create" {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			var body struct {
				Workspace string `json:"workspace"`
			}
			_ = json.Unmarshal(raw, &body)
			p.mu.Lock()
			p.creates = append(p.creates, verifyCreateCall{
				BodyWorkspace:   body.Workspace,
				HeaderWorkspace: r.Header.Get("X-Woca-Workspace"),
			})
			p.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}
		p.rp.ServeHTTP(w, r)
		return
	}
	httpx.WriteError(w, http.StatusNotFound, "unknown executor-proxy route")
}

// bindCalls / createCalls snapshot the recorded traffic.
func (p *verifyProxy) bindCalls() []verifyBindCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]verifyBindCall(nil), p.binds...)
}

func (p *verifyProxy) createCalls() []verifyCreateCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]verifyCreateCall(nil), p.creates...)
}

func verifyResp(t *testing.T, f *fixture, id string) (int, map[string]any, []byte) {
	t.Helper()
	resp := f.do("POST", "/api/sandboxes/"+id+"/verify", "", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("verify body: %v (%s)", err, raw)
		}
	}
	return resp.StatusCode, doc, raw
}

// createBackend registers one contract-http backend and returns its id.
func createBackend(t *testing.T, f *fixture, name, endpoint, key string) string {
	t.Helper()
	resp := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": name, "type": "contract-http", "endpoint": endpoint, "api_key": key,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create backend = %d", resp.StatusCode)
	}
	for _, r := range listSandboxes(t, f) {
		if r["name"] == name {
			return r["id"].(string)
		}
	}
	t.Fatal("backend not listed after create")
	return ""
}

func TestSandboxVerifyGreen(t *testing.T) {
	f := newFixture(t)
	fake := fakesandbox.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	px := newVerifyProxy(t, srv)
	pxSrv := httptest.NewServer(px)
	defer pxSrv.Close()

	id := createBackend(t, f, "fake", pxSrv.URL, "sb-verify-secret")
	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d", status)
	}
	if doc["ok"] != true {
		t.Fatalf("ok = %v (%s)", doc["ok"], raw)
	}
	if bytes.Contains(raw, []byte("sb-verify-secret")) {
		t.Fatalf("verify leaks the key: %s", raw)
	}
	steps, _ := doc["steps"].([]any)
	names := []string{}
	for _, st := range steps {
		m, _ := st.(map[string]any)
		if m["ok"] != true {
			t.Fatalf("step %v failed: %v", m["name"], m["error"])
		}
		names = append(names, m["name"].(string))
	}
	for _, want := range []string{"create", "exec", "file", "glob", "stat", "process", "archive"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("step %q missing from %v", want, names)
		}
	}
	if _, ok := doc["total_latency_ms"]; !ok {
		t.Fatal("total_latency_ms missing")
	}

	// The probe result persists on the row.
	row, ok, err := f.svc.getSandbox(id)
	if err != nil || !ok {
		t.Fatalf("row read: %v ok=%v", err, ok)
	}
	if !row.LastProbeOK || row.LastProbeAt == 0 || row.LastProbeError != "" {
		t.Fatalf("last_probe = %+v", row)
	}
}

// TestSandboxVerifyRoutesThroughProxy pins the reroute (spec §3.2): the
// probe never talks to the backend's endpoint directly — it binds a
// workspace through POST {endpoint}/internal/bindings with the fixed key
// verify:<backend-id>, then runs create→…→destroy through the proxy with
// that workspace stamped. Two probes of one backend bind the same key and
// therefore reuse one workspace: repeated verifies must not accumulate
// workspaces (the binding outlives each probe's destroy).
func TestSandboxVerifyRoutesThroughProxy(t *testing.T) {
	f := newFixture(t)
	fake := fakesandbox.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	px := newVerifyProxy(t, srv)
	pxSrv := httptest.NewServer(px)
	defer pxSrv.Close()

	id := createBackend(t, f, "px", pxSrv.URL, "sb-verify-secret")
	for round := 1; round <= 2; round++ {
		status, doc, raw := verifyResp(t, f, id)
		if status != http.StatusOK || doc["ok"] != true {
			t.Fatalf("verify round %d = %d ok=%v (%s)", round, status, doc["ok"], raw)
		}
	}

	binds := px.bindCalls()
	if len(binds) != 2 {
		t.Fatalf("bindings calls = %d want 2 (one per probe)", len(binds))
	}
	for i, b := range binds {
		if b.Tenant != "console" {
			t.Fatalf("bind %d tenant = %q want console", i, b.Tenant)
		}
		if want := "verify:" + id; b.Source != want {
			t.Fatalf("bind %d source_key = %q want %q", i, b.Source, want)
		}
	}
	creates := px.createCalls()
	if len(creates) != 2 {
		t.Fatalf("proxy-served creates = %d want 2", len(creates))
	}
	if creates[0].BodyWorkspace == "" || creates[0].HeaderWorkspace == "" {
		t.Fatalf("create not workspace-routed: %+v", creates[0])
	}
	if creates[0].BodyWorkspace != creates[0].HeaderWorkspace {
		t.Fatalf("create body/header workspace disagree: %+v", creates[0])
	}
	// Binding reuse: the second probe bound the same key, so the same
	// workspace answered — not a fresh ws-verify-<n+1> allocation.
	if creates[0].BodyWorkspace != creates[1].BodyWorkspace {
		t.Fatalf("verify workspaces diverged across probes: %q vs %q",
			creates[0].BodyWorkspace, creates[1].BodyWorkspace)
	}
	if !strings.HasPrefix(creates[0].BodyWorkspace, "ws-verify-") {
		t.Fatalf("workspace %q not minted by the proxy", creates[0].BodyWorkspace)
	}
	// The contract path ran through the proxy end to end: the upstream
	// (only reachable via the proxy) served both creates and both
	// destroys (Close of the contract provider).
	if fake.SyncCalls() != 0 {
		t.Fatalf("unexpected syncs: %d", fake.SyncCalls())
	}
}

func TestSandboxVerifyBadEndpoint(t *testing.T) {
	f := newFixture(t)
	// Port 1 on the loopback: connection refused — the first step (the
	// provider's create/provision call) must fail and the failure must
	// persist.
	id := createBackend(t, f, "dead", "http://127.0.0.1:1", "")

	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d", status)
	}
	if doc["ok"] != false {
		t.Fatalf("ok = %v want false (%s)", doc["ok"], raw)
	}
	steps, _ := doc["steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("no steps reported")
	}
	first, _ := steps[0].(map[string]any)
	if first["name"] != "create" || first["ok"] != false {
		t.Fatalf("first step = %v want failed create", first)
	}

	row, ok, err := f.svc.getSandbox(id)
	if err != nil || !ok {
		t.Fatalf("row read: %v", err)
	}
	if row.LastProbeOK || row.LastProbeAt == 0 || row.LastProbeError == "" {
		t.Fatalf("persisted failure = %+v", row)
	}
}

func TestSandboxVerifyUnregisteredType(t *testing.T) {
	f := newFixture(t)
	// e2b is in the type enum (registrable) but has no runtime provider:
	// verify must report the gap, not crash.
	resp := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "e2b", "type": "e2b", "endpoint": "https://e2b.example",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register e2b = %d", resp.StatusCode)
	}
	var id string
	for _, r := range listSandboxes(t, f) {
		if r["name"] == "e2b" {
			id = r["id"].(string)
		}
	}
	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d (%s)", status, raw)
	}
	if doc["ok"] != false {
		t.Fatalf("ok = %v want false", doc["ok"])
	}
	steps, _ := doc["steps"].([]any)
	first, _ := steps[0].(map[string]any)
	if first["name"] != "create" || first["ok"] != false {
		t.Fatalf("first step = %v", first)
	}
	if !strings.Contains(first["error"].(string), "not available") {
		t.Fatalf("error = %v want provider-not-available", first["error"])
	}
}
