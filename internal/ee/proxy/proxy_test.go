// proxy_test.go covers the control surfaces: allocate-or-bind (idempotency,
// tenant isolation, placement policy), the degraded scan loop, routing
// failure modes, and configuration validation.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/wocadb"
)

// openTestDB opens a fresh sqlite wocadb per test.
func openTestDB(t *testing.T) *wocadb.DB {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "proxy-test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedAck writes one ack row with the given age (role defaults to executor).
func seedAck(t *testing.T, db *wocadb.DB, id, addr string, load int64, age time.Duration) {
	t.Helper()
	seedAckRole(t, db, id, addr, load, age, "executor")
}

func seedAckRole(t *testing.T, db *wocadb.DB, id, addr string, load int64, age time.Duration, role string) {
	t.Helper()
	err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID:    id,
		Role:         role,
		Generation:   "g1",
		Addr:         addr,
		Capabilities: "sandbox",
		Load:         load,
		UpdatedAt:    wocadb.Now() - age.Milliseconds(),
	})
	if err != nil {
		t.Fatalf("seed ack %s: %v", id, err)
	}
}

func newProxyHandler(t *testing.T, db *wocadb.DB, l Limits) http.Handler {
	t.Helper()
	return New(Config{DB: db, Limits: l, Logger: zerolog.Nop()}).Handler()
}

// bindCall posts one allocate-or-bind request through h.
func bindCall(t *testing.T, h http.Handler, tenant, source string) (int, bindResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"tenant_id": tenant, "source_key": source})
	req := httptest.NewRequest(http.MethodPost, "/internal/bindings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out bindResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Errorf("bind response not json: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func bindingState(t *testing.T, db *wocadb.DB, ws string) string {
	t.Helper()
	b, ok, err := db.WorkspaceBindingByID(ws)
	if err != nil || !ok {
		t.Fatalf("binding %s: ok=%v err=%v", ws, ok, err)
	}
	return b.State
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBindAllocateIdempotentUnderConcurrency: concurrent allocate-or-bind
// calls over one source_key all observe the same workspace (one row).
func TestBindAllocateIdempotentUnderConcurrency(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	h := newProxyHandler(t, db, Limits{})

	const n = 16
	var wg sync.WaitGroup
	results := make([]bindResponse, n)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, out := bindCall(t, h, "t1", "t1|/home/u/proj")
			if code != http.StatusOK {
				t.Errorf("bind #%d: status %d, want 200", i, code)
			}
			results[i] = out
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if results[i].WorkspaceID == "" || results[i].WorkspaceID != results[0].WorkspaceID {
			t.Fatalf("bind #%d workspace %q, want %q (allocated exactly once)", i, results[i].WorkspaceID, results[0].WorkspaceID)
		}
		if results[i].ExecutorID != results[0].ExecutorID {
			t.Fatalf("bind #%d executor %q, want %q", i, results[i].ExecutorID, results[0].ExecutorID)
		}
	}
	rows, err := db.WorkspaceBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("workspace_bindings rows = %d, want 1", len(rows))
	}
}

// TestBindTenantIsolation: same source path under two tenants is two
// workspaces — the tenant scoping of source keys.
func TestBindTenantIsolation(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	h := newProxyHandler(t, db, Limits{})

	_, a := bindCall(t, h, "tenant-a", "tenant-a|/home/u/proj")
	_, b := bindCall(t, h, "tenant-b", "tenant-b|/home/u/proj")
	if a.WorkspaceID == "" || b.WorkspaceID == "" || a.WorkspaceID == b.WorkspaceID {
		t.Fatalf("tenant isolation broken: a=%q b=%q", a.WorkspaceID, b.WorkspaceID)
	}
}

// TestBindPlacementPolicy: load ASC first, then binding count ASC, then
// executor_id — deterministic across ties.
func TestBindPlacementPolicy(t *testing.T) {
	db := openTestDB(t)
	const addr = "http://127.0.0.1:9"
	seedAck(t, db, "exec-c", addr, 5, 0) // highest load: never first
	seedAck(t, db, "exec-b", addr, 1, 0)
	seedAck(t, db, "exec-a", addr, 1, 0)
	h := newProxyHandler(t, db, Limits{})

	// Load tie a/b, zero bindings each → lowest id.
	if _, got := bindCall(t, h, "t1", "t1|/s1"); got.ExecutorID != "exec-a" {
		t.Fatalf("placement 1: executor %q, want exec-a", got.ExecutorID)
	}
	// Load tie, exec-a now carries 1 binding → exec-b (0 bindings).
	if _, got := bindCall(t, h, "t1", "t1|/s2"); got.ExecutorID != "exec-b" {
		t.Fatalf("placement 2: executor %q, want exec-b", got.ExecutorID)
	}
	// Load and binding count tied (1 each) → lowest id again.
	if _, got := bindCall(t, h, "t1", "t1|/s3"); got.ExecutorID != "exec-a" {
		t.Fatalf("placement 3: executor %q, want exec-a", got.ExecutorID)
	}
	// A load drop beats binding counts: exec-c at load 0 wins with 0 bindings.
	seedAck(t, db, "exec-c", addr, 0, 0)
	if _, got := bindCall(t, h, "t1", "t1|/s4"); got.ExecutorID != "exec-c" {
		t.Fatalf("placement 4: executor %q, want exec-c", got.ExecutorID)
	}
}

// TestBindSkipsStaleExecutors: only fresh executor-role acks are candidates.
func TestBindSkipsStaleExecutors(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-stale", "http://127.0.0.1:9", 0, 20*time.Second)
	seedAck(t, db, "exec-live", "http://127.0.0.1:9", 9, 0)
	h := newProxyHandler(t, db, Limits{})
	if _, got := bindCall(t, h, "t1", "t1|/s"); got.ExecutorID != "exec-live" {
		t.Fatalf("placement picked %q, want exec-live (stale excluded)", got.ExecutorID)
	}
}

// TestBindNoHealthyExecutor: no fresh executor ack (a fresh worker ack does
// not qualify) → 503 with Retry-After.
func TestBindNoHealthyExecutor(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-dead", "http://127.0.0.1:9", 0, 30*time.Second)
	seedAckRole(t, db, "worker-1", "http://127.0.0.1:9", 0, 0, "worker")
	h := newProxyHandler(t, db, Limits{})

	req := httptest.NewRequest(http.MethodPost, "/internal/bindings", strings.NewReader(`{"tenant_id":"t1","source_key":"t1|/s"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bind status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "executor") {
		t.Fatalf("503 body lacks executor reason: %s", rec.Body.String())
	}
}

// TestBindInputValidation: malformed bodies and empty fields are 400s.
func TestBindInputValidation(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	h := newProxyHandler(t, db, Limits{})

	for name, body := range map[string]string{
		"malformed json": `{"tenant_id":`,
		"empty tenant":   `{"tenant_id":"","source_key":"t|/s"}`,
		"empty source":   `{"tenant_id":"t","source_key":""}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/internal/bindings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", name, rec.Code, rec.Body.String())
		}
	}
}

// TestBindReusesExistingBinding: an existing source key answers without
// re-placement (and without needing any healthy executor at all).
func TestBindReusesExistingBinding(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	h := newProxyHandler(t, db, Limits{})
	_, first := bindCall(t, h, "t1", "t1|/s")
	if first.WorkspaceID == "" {
		t.Fatal("first bind produced no workspace")
	}
	// Every executor goes stale — the existing binding still resolves.
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 30*time.Second)
	code, second := bindCall(t, h, "t1", "t1|/s")
	if code != http.StatusOK || second.WorkspaceID != first.WorkspaceID {
		t.Fatalf("re-bind: code=%d ws=%q, want 200 %q", code, second.WorkspaceID, first.WorkspaceID)
	}
}

// TestDegradedScanFlipsAndRecovers: ack out of window → degraded → requests
// 503 with a body naming the condition and the recovery; ack back in window
// → active → forwarding resumes.
func TestDegradedScanFlipsAndRecovers(t *testing.T) {
	db := openTestDB(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)
	seedAck(t, db, "exec-1", backend.URL, 0, 0)

	p := New(Config{DB: db, Limits: Limits{}, Logger: zerolog.Nop()})
	h := p.Handler()
	_, b := bindCall(t, h, "t1", "t1|/s")
	if b.State != wocadb.WorkspaceBindingActive {
		t.Fatalf("fresh binding state = %q, want active", b.State)
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	fwd := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sandbox/"+b.WorkspaceID+"/exec", strings.NewReader("{}"))
		req.Header.Set(WorkspaceHeader, b.WorkspaceID)
		req.Header.Set("Authorization", "Bearer k")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("forward: %v", err)
		}
		return resp
	}

	if resp := fwd(); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy forward status = %d, want 200", resp.StatusCode)
	}

	// Executor stops heartbeating: the scan flips the binding degraded.
	seedAck(t, db, "exec-1", backend.URL, 0, 20*time.Second)
	p.scanOnce()
	if got := bindingState(t, db, b.WorkspaceID); got != WorkspaceBindingDegraded {
		t.Fatalf("state after stale scan = %q, want degraded", got)
	}
	resp := fwd()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("degraded forward status = %d, want 503", resp.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	if !strings.Contains(errBody.Error, "degraded") || !strings.Contains(errBody.Error, "recover") {
		t.Fatalf("degraded error body = %q, want degraded + recovery condition", errBody.Error)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("degraded 503 without Retry-After")
	}

	// Executor acks again: the scan restores active and forwarding works.
	seedAck(t, db, "exec-1", backend.URL, 0, 0)
	p.scanOnce()
	if got := bindingState(t, db, b.WorkspaceID); got != wocadb.WorkspaceBindingActive {
		t.Fatalf("state after recovery scan = %q, want active", got)
	}
	if resp := fwd(); resp.StatusCode != http.StatusOK {
		t.Fatalf("recovered forward status = %d, want 200", resp.StatusCode)
	}
}

// TestDegradedScanLoop: Run drives the scan on its ticker.
func TestDegradedScanLoop(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	p := New(Config{DB: db, Limits: Limits{}, Logger: zerolog.Nop()})
	h := p.Handler()
	_, b := bindCall(t, h, "t1", "t1|/s")

	old := degradedScanEvery
	degradedScanEvery = 20 * time.Millisecond
	t.Cleanup(func() { degradedScanEvery = old })

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	defer cancel()

	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 20*time.Second)
	waitFor(t, "binding degraded by scan loop", func() bool {
		return bindingState(t, db, b.WorkspaceID) == WorkspaceBindingDegraded
	})
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	waitFor(t, "binding reactivated by scan loop", func() bool {
		return bindingState(t, db, b.WorkspaceID) == wocadb.WorkspaceBindingActive
	})
}

// TestRouteFailures: missing header, unknown workspace, stale executor ack.
func TestRouteFailures(t *testing.T) {
	db := openTestDB(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)
	seedAck(t, db, "exec-1", backend.URL, 0, 0)
	h := newProxyHandler(t, db, Limits{})
	_, b := bindCall(t, h, "t1", "t1|/s")

	cases := []struct {
		name      string
		workspace string
		wantIn    string
	}{
		{"missing header", "  ", "X-Woca-Workspace"},
		{"unknown workspace", "ws_ghost", "no workspace binding"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, "/sandbox/x/exec", strings.NewReader("{}"))
		req.Header.Set(WorkspaceHeader, tc.workspace)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", tc.name, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: 503 without Retry-After", tc.name)
		}
		if !strings.Contains(rec.Body.String(), tc.wantIn) {
			t.Errorf("%s: body %q lacks %q", tc.name, rec.Body.String(), tc.wantIn)
		}
	}

	// Binding active but executor ack out of window: fail closed even before
	// the scan flips the state.
	seedAck(t, db, "exec-1", backend.URL, 0, 20*time.Second)
	req, _ := http.NewRequest(http.MethodPost, "/sandbox/x/exec", strings.NewReader("{}"))
	req.Header.Set(WorkspaceHeader, b.WorkspaceID)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not live") {
		t.Fatalf("stale executor: code=%d body=%s, want 503 with not-live reason", rec.Code, rec.Body.String())
	}
}

// TestUnknownRoute404: surfaces outside the contract answer 404.
func TestUnknownRoute404(t *testing.T) {
	db := openTestDB(t)
	h := newProxyHandler(t, db, Limits{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// TestDatabaseFailuresFailClosed: bind, forward and scan all fail closed
// when the database breaks mid-flight.
func TestDatabaseFailuresFailClosed(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:9", 0, 0)
	p := New(Config{DB: db, Limits: Limits{}, Logger: zerolog.Nop()})
	h := p.Handler()
	if code, _ := bindCall(t, h, "t1", "t1|/s"); code != http.StatusOK {
		t.Fatalf("pre-close bind status %d", code)
	}
	// Break the database (closed handle: every read errors).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/bindings", strings.NewReader(`{"tenant_id":"t2","source_key":"t2|/s"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusInternalServerError {
		t.Fatalf("bind on closed DB: status %d, want 5xx", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/sandbox/ws/exec", nil)
	req.Header.Set(WorkspaceHeader, "ws")
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("forward on closed DB: status %d, want 503", rec.Code)
	}

	p.scanOnce() // must not panic on read failure
}

// TestProxyWithoutDatabase: a nil DB fail-closes every surface.
func TestProxyWithoutDatabase(t *testing.T) {
	h := New(Config{Limits: Limits{}, Logger: zerolog.Nop()}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/internal/bindings", strings.NewReader(`{"tenant_id":"t","source_key":"t|/s"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bind without DB: status %d, want 503", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/sandbox/ws/exec", nil)
	req.Header.Set(WorkspaceHeader, "ws")
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("forward without DB: status %d, want 503", rec.Code)
	}
}

// TestQueueTimeoutConfigWarn: a queue timeout at or beyond the sandbox
// client's default backend timeout (120s) draws a WARN — the proxy cannot
// observe the real backend timeout, so it can only flag the dangerous side.
func TestQueueTimeoutConfigWarn(t *testing.T) {
	db := openTestDB(t)
	var buf bytes.Buffer
	New(Config{DB: db, Limits: Limits{QueueTimeout: 130 * time.Second}, Logger: zerolog.New(&buf)})
	if !strings.Contains(buf.String(), "queue timeout") {
		t.Fatalf("expected queue-timeout WARN, log = %q", buf.String())
	}
	buf.Reset()
	New(Config{DB: db, Limits: Limits{QueueTimeout: 30 * time.Second}, Logger: zerolog.New(&buf)})
	if strings.Contains(buf.String(), "queue timeout") {
		t.Fatalf("unexpected WARN at 30s, log = %q", buf.String())
	}
}
