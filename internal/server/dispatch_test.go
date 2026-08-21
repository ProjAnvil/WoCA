package server

// Queue-dispatch tests (Phase 3 wiring): with DispatchToQueue, run creation
// enqueues a worker-executable run instead of executing in-process, SSE
// streams the ledger regardless, and the cross-instance session constraint
// surfaces as 409. RunsOffHost opens /api/runs to non-local tenants while
// every other host surface stays gated.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/worker"
	"github.com/rs/zerolog"
)

func newDispatchServer(t *testing.T, runsOffHost bool) (*Server, *runstate.Ledger, *httptest.Server) {
	t.Helper()
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: newScriptedAgent(t, "never-executed-here"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: ledger, DispatchToQueue: true, RunsOffHost: runsOffHost,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, ledger, srv
}

func dispatchPost(t *testing.T, url, sessionID, prompt string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"session_id": sessionID, "prompt": prompt})
	resp, err := http.Post(url+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A dispatched run is born queued with its execution environment in the
// config snapshot, and the local process never registers it.
func TestDispatchEnqueuesWorkerRun(t *testing.T) {
	s, ledger, srv := newDispatchServer(t, false)
	ws := t.TempDir()
	body, _ := json.Marshal(map[string]any{"session_id": "s-dispatch", "prompt": "queue me", "workspace": ws})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.RunID == "" {
		t.Fatal("no run id")
	}
	row, ok, err := ledger.Row(context.Background(), out.RunID)
	if err != nil || !ok {
		t.Fatalf("ledger row ok=%v err=%v", ok, err)
	}
	if row.Status != runstate.StatusQueued {
		t.Fatalf("status = %q want queued", row.Status)
	}
	if row.Owner != "" {
		t.Fatalf("queued row already owned by %q", row.Owner)
	}
	snap, err := runstate.ParseSnapshot(row.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Prompt != "queue me" || snap.SessionID != "s-dispatch" || snap.Workspace != ws || snap.ThreadID == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if r := s.lookupRun(out.RunID); r != nil {
		t.Fatal("dispatched run must not be registered locally (SSE must fall through to the ledger)")
	}
}

// The session's busy row marks the dispatched run, and the ledger SSE tail
// streams a worker-executed run to completion.
func TestDispatchSSEStreamsLedgerEvents(t *testing.T) {
	_, ledger, srv := newDispatchServer(t, false)
	resp := dispatchPost(t, srv.URL, "s-sse", "stream me")
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	// Simulate the worker: claim, mirror events, finish.
	ctx := context.Background()
	run, err := ledger.Claim(ctx, "worker-test", time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim = %v, %v", run, err)
	}
	if run.RunID != out.RunID {
		t.Fatalf("claimed %q, dispatched %q", run.RunID, out.RunID)
	}
	for _, ev := range []struct {
		kind, payload string
	}{
		{"run_started", `{}`},
		{"delta", `{"text":"worker reply"}`},
	} {
		if _, err := ledger.Append(ctx, out.RunID, runstate.Event{Kind: ev.kind, Payload: ev.payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Finish(ctx, out.RunID, "worker-test", "done"); err != nil {
		t.Fatal(err)
	}
	_, _ = ledger.Append(ctx, out.RunID, runstate.Event{Kind: "done", Payload: `{"cancelled":false}`})

	frames := followSSE(t, srv.URL+"/api/runs/"+out.RunID+"/events", "")
	if len(frames) == 0 {
		t.Fatal("no SSE frames")
	}
	last := frames[len(frames)-1]
	if last.event != "done" {
		t.Fatalf("terminal frame = %q", last.event)
	}
}

// A second dispatch for a session with an active run is rejected with 409
// (the ledger's one_active_run_per_session constraint is the gate).
func TestDispatchRejectsActiveSession(t *testing.T) {
	_, _, srv := newDispatchServer(t, false)
	resp := dispatchPost(t, srv.URL, "s-busy", "first")
	resp.Body.Close()
	second := dispatchPost(t, srv.URL, "s-busy", "second")
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d want 409", second.StatusCode)
	}
}

// tenantProvider authenticates every credential as one fixed tenant.
type tenantProvider struct{ tenant string }

func (p tenantProvider) Authenticate(context.Context, string) (tenancy.Context, error) {
	return tenancy.Context{TenantID: p.tenant}, nil
}

// RunsOffHost admits a non-local tenant to /api/runs but keeps every other
// host surface forbidden; without it the run surface stays local-gated.
func TestRunsOffHostGate(t *testing.T) {
	for _, tc := range []struct {
		offHost              bool
		runStatus, setStatus int
	}{
		{false, http.StatusForbidden, http.StatusForbidden},
		{true, http.StatusOK, http.StatusForbidden},
	} {
		ledger := openRunLedger(t)
		s := New(Config{
			Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
			SessionDir: t.TempDir(), WorkDir: t.TempDir(),
			RunLedger: ledger, DispatchToQueue: true, RunsOffHost: tc.offHost,
			Auth: tenantProvider{tenant: "tenant-a"},
		})
		srv := httptest.NewServer(s.Handler())
		run := dispatchPost(t, srv.URL, "s-remote", "from afar")
		run.Body.Close()
		if run.StatusCode != tc.runStatus {
			t.Fatalf("offHost=%v: /api/runs status = %d want %d", tc.offHost, run.StatusCode, tc.runStatus)
		}
		settingsResp, err := http.Get(srv.URL + "/api/settings")
		if err != nil {
			t.Fatal(err)
		}
		settingsResp.Body.Close()
		if settingsResp.StatusCode != tc.setStatus {
			t.Fatalf("offHost=%v: /api/settings status = %d want %d", tc.offHost, settingsResp.StatusCode, tc.setStatus)
		}
		srv.Close()
	}
}

// TestDispatchWorkerEndToEnd proves the Phase 3 wiring acceptance shape: a
// run created on the gateway lands in the queue, a worker (separate
// process role, same database) claims and executes it, and a client
// attached to the gateway follows the whole stream over ledger SSE.
func TestDispatchWorkerEndToEnd(t *testing.T) {
	_, ledger, srv := newDispatchServer(t, false)
	resp := dispatchPost(t, srv.URL, "s-e2e", "run on the worker")
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	a := newScriptedAgent(t, "worker executed this")
	bus := eventbus.NewLocalWithRetention(16)
	t.Cleanup(func() { _ = bus.Close() })
	w := worker.New(worker.Config{
		Ledger: ledger, Agent: a, Bus: bus, Logger: zerolog.Nop(), ID: "w-e2e",
	})
	go func() { _ = w.Step(context.Background()) }()

	frames := followSSE(t, srv.URL+"/api/runs/"+out.RunID+"/events", "")
	var delta, done bool
	for _, f := range frames {
		switch f.event {
		case "delta":
			delta = true
		case "done":
			done = true
		}
	}
	if !delta || !done {
		t.Fatalf("frames = delta:%v done:%v", delta, done)
	}
	row, _, _ := ledger.Row(context.Background(), out.RunID)
	if row.Status != "done" || row.Owner != "w-e2e" {
		t.Fatalf("terminal row = %+v", row)
	}
}

// Message-plane settings round-trip through the settings API and apply live
// (no restart flag): the plane resolver reads the store per operation.
func TestMsgPlaneSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := settings.Open("sqlite:" + filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: dir, WorkDir: dir, Settings: st,
		RunLedger: openRunLedger(t), DispatchToQueue: true,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Set pg+nats with a URL.
	body, _ := json.Marshal(map[string]any{
		"msg_plane_mode": "pg+nats", "msg_plane_nats_url": "nats://plane:4222",
	})
	resp, err := http.NewRequest(http.MethodPut, srv.URL+"/api/settings", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	put, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK              bool `json:"ok"`
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.NewDecoder(put.Body).Decode(&out)
	put.Body.Close()
	if !out.OK || out.RestartRequired {
		t.Fatalf("update = ok:%v restart:%v (message plane must apply live)", out.OK, out.RestartRequired)
	}

	get, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	var cfg struct {
		Mode    string `json:"msg_plane_mode"`
		NATSURL string `json:"msg_plane_nats_url"`
	}
	_ = json.NewDecoder(get.Body).Decode(&cfg)
	if cfg.Mode != "pg+nats" || cfg.NATSURL != "nats://plane:4222" {
		t.Fatalf("msg plane settings = %+v", cfg)
	}
	// The store the resolver reads already carries the new plane.
	if c := st.Load(); c.MsgPlaneMode != "pg+nats" || c.MsgPlaneNATSURL != "nats://plane:4222" {
		t.Fatalf("store = %+v", c)
	}

	// A personal edition (no queue dispatch) rejects the enterprise
	// message-plane keys instead of storing no-op configuration.
	personal := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: dir, WorkDir: dir, Settings: st,
	})
	psrv := httptest.NewServer(personal.Handler())
	defer psrv.Close()
	pget, err := http.Get(psrv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	var pcfg struct {
		Available bool `json:"msg_plane_available"`
	}
	_ = json.NewDecoder(pget.Body).Decode(&pcfg)
	pget.Body.Close()
	if pcfg.Available {
		t.Fatal("personal edition must not report the message plane available")
	}
	pbody, _ := json.Marshal(map[string]any{"msg_plane_mode": "pg+nats"})
	preq, _ := http.NewRequest(http.MethodPut, psrv.URL+"/api/settings", bytes.NewReader(pbody))
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusBadRequest {
		t.Fatalf("personal edition msg-plane write status = %d want 400", presp.StatusCode)
	}

	// Invalid mode values are rejected.
	bad, _ := json.Marshal(map[string]any{"msg_plane_mode": "nats-only"})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/settings", bytes.NewReader(bad))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d want 400", r2.StatusCode)
	}
}
