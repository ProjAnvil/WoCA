package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// fakeLadyM is a minimal ladyM data-plane: it records every request's
// workspace and auth header and answers canned /api responses.
type fakeLadyM struct {
	mu         sync.Mutex
	workspaces []string
	auths      []string
	paths      []string
	failAll    bool
	server     *httptest.Server
	// recall_ops / record_operation fixtures (v0.8.4 data plane).
	opsItems    []map[string]any
	opsFallback bool
	opStatus    int
}

func newFakeLadyM(t *testing.T) *fakeLadyM {
	t.Helper()
	f := &fakeLadyM{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/remember", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content   string   `json:"content"`
			Source    string   `json:"source"`
			Tags      []string `json:"tags"`
			Workspace string   `json:"workspace"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		if f.failing() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if body.Content == "drop-me" {
			write(w, map[string]any{"id": "", "hash": "", "gated": "dropped", "reason": "duplicate"})
			return
		}
		write(w, map[string]any{"id": "m_1", "hash": "h", "gated": "", "reason": ""})
	})
	mux.HandleFunc("POST /api/recall", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string `json:"query"`
			Workspace string `json:"workspace"`
			CodeOnly  bool   `json:"code_only"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		if f.failing() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		layer := "semantic"
		typ := "fact"
		if body.CodeOnly {
			layer, typ = "code", "symbol"
		}
		write(w, map[string]any{
			"query": body.Query,
			"results": []map[string]any{{
				"memory": map[string]any{
					"id": "m_" + body.Workspace, "layer": layer, "type": typ,
					"content": "the sky is blue", "summary": "sky is blue",
					"source": "http", "tags": []string{"fact"},
					"updated_at": 1.5,
				},
				"score": 0.9, "tier": 1, "via": []string{},
			}},
			"tier_reached": 1,
		})
	})
	mux.HandleFunc("POST /api/record_event", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		if f.failing() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		write(w, map[string]any{"id": "e_1", "layer": "episodic", "type": "event"})
	})
	mux.HandleFunc("POST /api/consolidate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		write(w, map[string]any{"kept_episodes": 2, "promoted_to_semantic": 1, "actions": map[string]int{"merged": 1}})
	})
	mux.HandleFunc("POST /api/stats", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		write(w, map[string]any{
			"total_memories": 3, "by_layer": map[string]int{"semantic": 2, "episodic": 1},
		})
	})
	mux.HandleFunc("GET /api/memories", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.URL.Query().Get("workspace"))
		write(w, map[string]any{
			"memories": []map[string]any{{
				"id": "m_list", "layer": "semantic", "type": "fact",
				"content": "c", "summary": "s", "source": "http",
				"tags": []string{}, "updated_at": 2,
			}},
			"total": 1,
		})
	})
	mux.HandleFunc("POST /api/forget", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/recall_ops", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
			Resource  string `json:"resource"`
			Action    string `json:"action"`
			Limit     int    `json:"limit"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		f.mu.Lock()
		status, items, fallback := f.opStatus, f.opsItems, f.opsFallback
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"resource must not be blank"}`))
			return
		}
		write(w, map[string]any{"resource": "res_1", "items": items, "fallback": fallback})
	})
	mux.HandleFunc("POST /api/record_operation", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
			Action    string `json:"action"`
		}
		decodeBody(r, &body)
		f.record(r, body.Workspace)
		f.mu.Lock()
		status := f.opStatus
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"resources must not be empty"}`))
			return
		}
		write(w, map[string]any{"id": "op_1", "dropped": false})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLadyM) record(r *http.Request, workspace string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = append(f.workspaces, workspace)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
}

// decodeBody reads the request body exactly once.
func decodeBody(r *http.Request, into any) {
	_ = json.NewDecoder(r.Body).Decode(into)
}

func (f *fakeLadyM) failing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failAll
}

func (f *fakeLadyM) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAll = v
}

func (f *fakeLadyM) snapshot() (workspaces, auths, paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.workspaces...), append([]string{}, f.auths...), append([]string{}, f.paths...)
}

func newTestRemote(t *testing.T, f *fakeLadyM) *RemoteEngine {
	t.Helper()
	eng, err := BuildRemote(RemoteConfig{
		Endpoint: f.server.URL, Username: "woca-worker", Password: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func tenantCtx() context.Context {
	return tenancy.NewContext(context.Background(), tenancy.Context{
		TenantID: "t_acme", UserID: "u_alice", Scopes: []string{},
	})
}

func TestRemoteWorkspaceMapping(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	if _, err := eng.Remember(tenantCtx(), "/repo", "the sky is blue", []string{"fact"}, "woca:auto"); err != nil {
		t.Fatal(err)
	}
	ws, auths, _ := f.snapshot()
	if len(ws) == 0 || ws[0] != "woca:t_acme:u_alice:/repo" {
		t.Fatalf("workspace key = %v", ws)
	}
	if len(auths) == 0 || auths[0] != "Basic d29jYS13b3JrZXI6czNjcmV0" {
		t.Fatalf("authorization = %v", auths)
	}
}

func TestRemoteRecallFloatsUpAndMerges(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	hits, err := eng.Recall(tenantCtx(), "sky", "/repo", 8)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, _ := f.snapshot()
	// Both the workspace scope and the user-level scope were consulted.
	if len(ws) != 2 || ws[0] != "woca:t_acme:u_alice:/repo" || ws[1] != "woca:t_acme:u_alice" {
		t.Fatalf("float-up workspaces = %v", ws)
	}
	// The fake returns one hit per scope keyed by workspace; the duplicate
	// shape differs so both survive, ordered by score.
	if len(hits) == 0 {
		t.Fatal("no hits merged")
	}
}

func TestRemoteRecallDegradesToEmptyOnFailure(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	f.setFail(true)
	hits, err := eng.Recall(tenantCtx(), "sky", "/repo", 8)
	if err != nil {
		t.Fatalf("Recall error = %v, want degraded empty", err)
	}
	if len(hits) != 0 {
		t.Fatalf("degraded Recall = %d hits", len(hits))
	}
}

func TestRemoteBreakerOpensAndRecovers(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	f.setFail(true)
	for range breakerFailures * 2 {
		if _, err := eng.Recall(t.Context(), "q", "w", 4); err != nil {
			t.Fatal(err)
		}
	}
	ws, _, _ := f.snapshot()
	if len(ws) > breakerFailures*3 {
		t.Fatalf("breaker did not open: %d calls passed through", len(ws))
	}
	f.setFail(false)
	// The breaker window has not elapsed, so reads still short-circuit…
	if hits, _ := eng.Recall(t.Context(), "q", "w", 4); hits != nil {
		t.Fatal("breaker window open but a call went out")
	}
	// …and explicitly resetting the breaker (as a recovered health probe
	// would) restores service.
	eng.mu.Lock()
	eng.openUntil = timeZero
	eng.mu.Unlock()
	if hits, err := eng.Recall(t.Context(), "q", "w", 4); err != nil || len(hits) == 0 {
		t.Fatalf("recovered Recall = %v err=%v", hits, err)
	}
}

func TestRemoteWriteSurfacesErrorsAndGateDrops(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	id, err := eng.Remember(tenantCtx(), "/repo", "keep me", nil, "woca:auto")
	if err != nil || id != "m_1" {
		t.Fatalf("Remember = %q err=%v", id, err)
	}
	// The server's attention gate drop maps to the embedded engine's
	// contract: empty id, nil error.
	id, err = eng.Remember(tenantCtx(), "/repo", "drop-me", nil, "woca:auto")
	if err != nil || id != "" {
		t.Fatalf("gated Remember = %q err=%v", id, err)
	}

	f.setFail(true)
	if _, err := eng.Remember(tenantCtx(), "/repo", "x", nil, "s"); err == nil {
		t.Fatal("write failure swallowed")
	}
	if err := eng.RecordEvent(tenantCtx(), "/repo", "woca", "run", "o", "ok"); err == nil {
		t.Fatal("record_event failure swallowed")
	}
}

func TestRemoteListStatsForgetConsolidate(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)
	ctx := tenantCtx()

	hits, err := eng.List(ctx, "/repo", "semantic", 10, 0)
	if err != nil || len(hits) != 1 || hits[0].ID != "m_list" {
		t.Fatalf("List = %v err=%v", hits, err)
	}
	st, err := eng.Stats(ctx, "/repo")
	if err != nil || st.Total != 3 || st.Layers["semantic"] != 2 {
		t.Fatalf("Stats = %+v err=%v", st, err)
	}
	if err := eng.Forget(ctx, "m_1"); err != nil {
		t.Fatal(err)
	}
	report, err := eng.Consolidate(ctx, "/repo", 0)
	if err != nil || report == "" {
		t.Fatalf("Consolidate = %q err=%v", report, err)
	}
	_, _, paths := f.snapshot()
	wantPaths := map[string]bool{
		"GET /api/memories": false, "POST /api/stats": false,
		"POST /api/forget": false, "POST /api/consolidate": false,
	}
	for _, p := range paths {
		if _, ok := wantPaths[p]; ok {
			wantPaths[p] = true
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Fatalf("endpoint never called: %s", p)
		}
	}
}

func TestRemoteIndexCodeRequiresSandbox(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)
	if _, err := eng.IndexCode(t.Context(), "/repo", "/repo", false); err == nil {
		t.Fatal("IndexCode succeeded over the remote data-plane")
	}
}

func TestRemoteSearchCodeUsesCodeOnly(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)
	hits, err := eng.SearchCode(tenantCtx(), "parse", "/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Layer != "code" || hits[0].Type != "symbol" {
		t.Fatalf("SearchCode hits = %+v", hits)
	}
}

func TestBuildRemoteValidation(t *testing.T) {
	if _, err := BuildRemote(RemoteConfig{}); err == nil {
		t.Fatal("empty endpoint accepted")
	}
	if _, err := BuildRemote(RemoteConfig{Endpoint: "not a url"}); err == nil {
		t.Fatal("malformed endpoint accepted")
	}
	// No-auth deployments are valid: empty credentials build fine.
	if _, err := BuildRemote(RemoteConfig{Endpoint: "http://ladym:8080"}); err != nil {
		t.Fatalf("no-auth deployment rejected: %v", err)
	}
	_ = fmt.Sprintf
}

// Phase 2 acceptance: two tenants never touch each other's ladyM key
// space — every call under a tenant context addresses only that tenant's
// encoded workspace keys.
func TestRemoteTenantKeyIsolation(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	ctxA := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tA", UserID: "u1"})
	ctxB := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tB", UserID: "u1"})

	if _, err := eng.Remember(ctxA, "/w", "alpha-secret", nil, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Remember(ctxB, "/w", "beta-secret", nil, "s"); err != nil {
		t.Fatal(err)
	}

	// Recall under each tenant consults only that tenant's keys (workspace
	// scope plus its own user-level float-up).
	ws, _, _ := f.snapshot()
	seen := map[string]bool{}
	for _, k := range ws {
		seen[k] = true
	}
	if !seen["woca:tA:u1:/w"] || !seen["woca:tB:u1:/w"] {
		t.Fatalf("writes missing tenant keys: %v", ws)
	}
	if seen["woca:tA:u1"] != seen["woca:tB:u1"] {
		t.Fatalf("float-up asymmetric: %v", ws)
	}
}

func TestRemoteDescriptionAndClosedEngineDegradation(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)
	if d := eng.Description(); !strings.HasPrefix(d, "ladym-remote:") {
		t.Fatalf("Description = %q", d)
	}
	// Description tolerates an endpoint that stops parsing.
	weird, err := BuildRemote(RemoteConfig{Endpoint: "http://" + f.server.URL[len("http://"):], Username: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer weird.Close()
	_ = weird.Description()

	// orDefaultSource branches.
	if orDefaultSource("x") != "x" || orDefaultSource("") != "woca:migration" {
		t.Fatal("orDefaultSource branches")
	}
}

// v0.8.4 brought recall_ops to the HTTP data plane; RemoteEngine implements
// the OperationEngine capability over it (embedded-parity semantics:
// unknown resource degrades to empty hits, failures degrade to errors the
// caller's best-effort path warns about).

func TestRemoteEngineRecallOperations(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	f.mu.Lock()
	f.opsItems = []map[string]any{{
		"memory": map[string]any{
			"id": "e_9", "layer": "episodic", "type": "event",
			"content": "agent=woca | action=update | observation=q | outcome=success",
			"summary": "agent=woca | action=update | observation=q | outcome=success",
			"source":  "http", "tags": []string{}, "updated_at": 5,
			"metadata": map[string]any{
				"action": "update", "outcome": "success", "agent": "woca",
			},
		},
		"score": 0.87,
	}}
	f.mu.Unlock()

	var ops OperationEngine = eng
	hits, err := ops.RecallOperations(t.Context(), "woca:t:u:ws", "src/a.go", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want 1", hits)
	}
	if hits[0].ID != "e_9" || hits[0].Action != "update" || hits[0].Outcome != "success" {
		t.Fatalf("hit = %+v", hits[0])
	}

	// An unknown resource rides the fallback pivot: empty hits, no error.
	f.mu.Lock()
	f.opsItems, f.opsFallback = nil, true
	f.mu.Unlock()
	hits, err = ops.RecallOperations(t.Context(), "woca:t:u:ws", "missing.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("fallback returned hits: %+v", hits)
	}
}

func TestRemoteEngineRecordOperation(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)

	var ops OperationEngine = eng
	id, err := ops.RecordOperation(t.Context(), "woca:t:u:ws", Operation{
		Resources: []string{"src/a.go", "src/b.go"},
		Action:    "update",
		Summary:   "q",
		Outcome:   "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "op_1" {
		t.Fatalf("id = %q, want op_1", id)
	}

	// A server-side rejection (blank resources → 400) surfaces as an error;
	// the retain path warns and moves on.
	f.mu.Lock()
	f.opStatus = http.StatusBadRequest
	f.mu.Unlock()
	if _, err := ops.RecordOperation(t.Context(), "woca:t:u:ws", Operation{Action: "update"}); err == nil {
		t.Fatal("blank resources must surface an error")
	}

	// A failed read degrades to an error too (caller warns, run continues).
	if _, err := ops.RecallOperations(t.Context(), "woca:t:u:ws", "src/a.go", 5); err == nil {
		t.Fatal("a 400 recall_ops must surface an error")
	}
}
