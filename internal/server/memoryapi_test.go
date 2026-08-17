package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/memory"
)

type fakeMemEngine struct {
	mu         sync.Mutex
	hits       []memory.Hit
	remembered []string
	listed     []memory.Hit
	forgotten  []string
	drop       bool // Remember returns ("", nil): the attention gate dropped it
	// Optional error injections for the failure branches of each verb.
	recallErr   error
	listErr     error
	statsErr    error
	rememberErr error
	forgetErr   error

	lastRecall struct {
		query string
		ws    string
		topK  int
	}
	lastList struct {
		ws     string
		layer  string
		limit  int
		offset int
	}
}

func (f *fakeMemEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	f.mu.Lock()
	f.lastRecall.query, f.lastRecall.ws, f.lastRecall.topK = q, ws, k
	err := f.recallErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.hits, nil
}
func (f *fakeMemEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeMemEngine) Remember(ctx context.Context, ws, c string, tags []string, src string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remembered = append(f.remembered, ws+"|"+src+"|"+c)
	if f.rememberErr != nil {
		return "", f.rememberErr
	}
	if f.drop {
		return "", nil
	}
	return "srv-1", nil
}
func (f *fakeMemEngine) RecordEvent(ctx context.Context, ws, a, act, obs, o string) error { return nil }
func (f *fakeMemEngine) List(ctx context.Context, ws string, layer string, limit, off int) ([]memory.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastList.ws, f.lastList.layer, f.lastList.limit, f.lastList.offset = ws, layer, limit, off
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

// recordedRecall returns the last Recall call's arguments.
func (f *fakeMemEngine) recordedRecall() (query, ws string, topK int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRecall.query, f.lastRecall.ws, f.lastRecall.topK
}

// recordedList returns the last List call's arguments.
func (f *fakeMemEngine) recordedList() (ws, layer string, limit, offset int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastList.ws, f.lastList.layer, f.lastList.limit, f.lastList.offset
}
func (f *fakeMemEngine) Forget(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, id)
	return f.forgetErr
}
func (f *fakeMemEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return &memory.Stats{Total: 2, Layers: map[string]int{"L2_semantic": 2}}, nil
}
func (f *fakeMemEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "ok", nil
}
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string) (string, error) { return "ok", nil }
func (f *fakeMemEngine) Description() string                                        { return "ladym test.db" }
func (f *fakeMemEngine) Close() error                                               { return nil }

func newMemoryTestServer(t *testing.T, eng memory.Engine) *httptest.Server {
	t.Helper()
	s := New(Config{MemoryEngine: eng, WorkDir: "/ws/default", Logger: zerolog.Nop()})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestMemoryListAndSearch(t *testing.T) {
	f := &fakeMemEngine{
		hits:   []memory.Hit{{ID: "recall-1", Layer: "L2_semantic", Type: "fact", Summary: "JWT auth flow", UpdatedAt: 1}},
		listed: []memory.Hit{{ID: "list-1", Layer: "L2_semantic", Type: "fact", Summary: "prefers tabs", UpdatedAt: 1}},
	}
	srv := newMemoryTestServer(t, f)
	// no q -> List
	resp, err := http.Get(srv.URL + "/api/memory?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Engine string       `json:"engine"`
		Items  []memory.Hit `json:"items"`
		Total  int          `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "ladym" || len(body.Items) != 1 || body.Items[0].ID != "list-1" || body.Total != 1 {
		t.Fatalf("list body = %+v", body)
	}
	// with q -> Recall (a DIFFERENT item than List returned)
	resp2, err := http.Get(srv.URL + "/api/memory?workspace=/ws&q=auth")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var b2 struct {
		Items []memory.Hit `json:"items"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&b2); err != nil {
		t.Fatal(err)
	}
	if len(b2.Items) != 1 || b2.Items[0].ID != "recall-1" {
		t.Fatalf("search body = %+v", b2)
	}
	if q, ws, k := f.recordedRecall(); q != "auth" || ws != "/ws" || k != 20 {
		t.Fatalf("recorded recall = q %q ws %q topK %d", q, ws, k)
	}
}

func TestMemoryStatsPostDelete(t *testing.T) {
	f := &fakeMemEngine{}
	srv := newMemoryTestServer(t, f)

	resp, err := http.Get(srv.URL + "/api/memory/stats?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Engine    string        `json:"engine"`
		Stats     *memory.Stats `json:"stats"`
		Workspace string        `json:"workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Engine != "ladym" || st.Stats == nil || st.Stats.Total != 2 {
		t.Fatalf("stats = %+v", st)
	}

	postResp, err := http.Post(srv.URL+"/api/memory", "application/json",
		strings.NewReader(`{"workspace":"/ws","content":"prefers tabs","tags":["manual"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(postResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "srv-1" {
		t.Fatalf("created = %+v", created)
	}
	if len(f.remembered) != 1 || f.remembered[0] != "/ws|woca:panel|prefers tabs" {
		t.Fatalf("remembered = %v", f.remembered)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/memory/srv-1", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", delResp.StatusCode)
	}
	if len(f.forgotten) != 1 || f.forgotten[0] != "srv-1" {
		t.Fatalf("forgotten = %v", f.forgotten)
	}
}

func TestMemoryDisabledDegrades(t *testing.T) {
	srv := newMemoryTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/api/memory?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Engine string       `json:"engine"`
		Items  []memory.Hit `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "none" || body.Items == nil {
		t.Fatalf("engine = %q items=%v", body.Engine, body.Items)
	}
	// POST on disabled engine -> 503
	postResp, err := http.Post(srv.URL+"/api/memory", "application/json", strings.NewReader(`{"content":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled POST status = %d", postResp.StatusCode)
	}
}

func TestMemoryDefaultsWorkspaceAndLimits(t *testing.T) {
	f := &fakeMemEngine{listed: []memory.Hit{{ID: "x"}}}
	srv := newMemoryTestServer(t, f)
	resp, err := http.Get(srv.URL + "/api/memory") // no workspace param
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Items []memory.Hit `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected default-workspace list, got %+v", body)
	}
	// Missing workspace falls back to the server workdir; missing limit
	// defaults to 20.
	if ws, _, limit, _ := f.recordedList(); ws != "/ws/default" || limit != 20 {
		t.Fatalf("default list call = ws %q limit %d, want /ws/default 20", ws, limit)
	}
	// bad limit clamps back to 20 (workspace still defaults to workdir)
	resp2, err := http.Get(srv.URL + "/api/memory?limit=9999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("clamped limit status = %d", resp2.StatusCode)
	}
	if ws, _, limit, _ := f.recordedList(); ws != "/ws/default" || limit != 20 {
		t.Fatalf("clamped list call = ws %q limit %d, want /ws/default 20", ws, limit)
	}
}

func TestMemoryRememberGateDrop(t *testing.T) {
	f := &fakeMemEngine{drop: true}
	srv := newMemoryTestServer(t, f)
	resp, err := http.Post(srv.URL+"/api/memory", "application/json",
		strings.NewReader(`{"workspace":"/ws","content":"recent duplicate"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID      string `json:"id"`
		Dropped bool   `json:"dropped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "" || !created.Dropped {
		t.Fatalf("gate drop = %+v, want empty id + dropped=true", created)
	}
	// The engine was still consulted (and recorded the attempt).
	if len(f.remembered) != 1 || f.remembered[0] != "/ws|woca:panel|recent duplicate" {
		t.Fatalf("remembered = %v", f.remembered)
	}
}

func TestMemoryRememberBodyCap(t *testing.T) {
	f := &fakeMemEngine{}
	srv := newMemoryTestServer(t, f)
	big := `{"content":"` + strings.Repeat("a", 1<<20+8) + `"}`
	resp, err := http.Post(srv.URL+"/api/memory", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", resp.StatusCode)
	}
	if len(f.remembered) != 0 {
		t.Fatalf("oversized body must not reach the engine: %v", f.remembered)
	}
}

func TestMemoryEngineErrorPaths(t *testing.T) {
	f := &fakeMemEngine{
		recallErr:   errors.New("recall boom"),
		listErr:     errors.New("list boom"),
		statsErr:    errors.New("stats boom"),
		rememberErr: errors.New("remember boom"),
		forgetErr:   errors.New("forget boom"),
	}
	srv := newMemoryTestServer(t, f)

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/api/memory"); code != http.StatusInternalServerError {
		t.Fatalf("list error status = %d, want 500", code)
	}
	if code := get("/api/memory?q=x"); code != http.StatusInternalServerError {
		t.Fatalf("recall error status = %d, want 500", code)
	}
	if code := get("/api/memory/stats"); code != http.StatusInternalServerError {
		t.Fatalf("stats error status = %d, want 500", code)
	}

	resp, err := http.Post(srv.URL+"/api/memory", "application/json",
		strings.NewReader(`{"content":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("remember error status = %d, want 500", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/memory/srv-1", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("forget error status = %d, want 500", delResp.StatusCode)
	}
}

func TestMemoryListEmptyItemsAndOffsetClamp(t *testing.T) {
	// An engine returning nothing yields [] items, never null.
	f := &fakeMemEngine{}
	srv := newMemoryTestServer(t, f)
	resp, err := http.Get(srv.URL + "/api/memory?offset=-5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Items []memory.Hit `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Items == nil || body.Total != 0 {
		t.Fatalf("empty list body = %+v", body)
	}
	// A negative offset clamps to zero before reaching the engine.
	if _, _, _, off := f.recordedList(); off != 0 {
		t.Fatalf("offset = %d, want clamped 0", off)
	}
}

func TestMemoryStatsAndForgetDisabled(t *testing.T) {
	srv := newMemoryTestServer(t, nil)
	// Stats degrade to engine "none" instead of 503.
	resp, err := http.Get(srv.URL + "/api/memory/stats?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Engine    string `json:"engine"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Engine != "none" || st.Workspace != "/ws" {
		t.Fatalf("disabled stats = %+v", st)
	}
	// Forgetting without an engine is a 503.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/memory/x", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled forget status = %d, want 503", delResp.StatusCode)
	}
}

func TestMemoryRememberValidation(t *testing.T) {
	f := &fakeMemEngine{}
	srv := newMemoryTestServer(t, f)

	resp, err := http.Post(srv.URL+"/api/memory", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", resp.StatusCode)
	}
	resp, err = http.Post(srv.URL+"/api/memory", "application/json", strings.NewReader(`{"content":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank content status = %d, want 400", resp.StatusCode)
	}
	if len(f.remembered) != 0 {
		t.Fatalf("invalid requests reached the engine: %v", f.remembered)
	}
}

func TestMemoryEngineResolvedThroughAgent(t *testing.T) {
	// With an agent wired, the engine is resolved live from the agent (the
	// construction-time snapshot is the fallback only).
	f := &fakeMemEngine{listed: []memory.Hit{{ID: "live-1"}}}
	a := newTestAgent(t)
	a.SetMemoryEngine(f)
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/memory?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Engine string       `json:"engine"`
		Items  []memory.Hit `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "ladym" || len(body.Items) != 1 || body.Items[0].ID != "live-1" {
		t.Fatalf("agent-resolved engine body = %+v", body)
	}
}
