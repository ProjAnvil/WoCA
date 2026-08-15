package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/memory"
)

type fakeMemEngine struct {
	mu         sync.Mutex
	hits       []memory.Hit
	remembered []string
	listed     []memory.Hit
	forgotten  []string
}

func (f *fakeMemEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return f.hits, nil
}
func (f *fakeMemEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeMemEngine) Remember(ctx context.Context, ws, c string, tags []string, src string) (string, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.remembered = append(f.remembered, ws+"|"+src+"|"+c)
	return "srv-1", nil
}
func (f *fakeMemEngine) RecordEvent(ctx context.Context, ws, a, act, obs, o string) error { return nil }
func (f *fakeMemEngine) List(ctx context.Context, ws, layer string, limit, off int) ([]memory.Hit, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	return f.listed, nil
}
func (f *fakeMemEngine) Forget(ctx context.Context, id string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, id)
	return nil
}
func (f *fakeMemEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 2, Layers: map[string]int{"L2_semantic": 2}}, nil
}
func (f *fakeMemEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "ok", nil
}
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string) (string, error) { return "ok", nil }
func (f *fakeMemEngine) Description() string                                      { return "ladym test.db" }
func (f *fakeMemEngine) Close() error                                             { return nil }

func newMemoryTestServer(t *testing.T, eng memory.Engine) *httptest.Server {
	t.Helper()
	s := New(Config{MemoryEngine: eng, WorkDir: "/ws/default", Logger: zerolog.Nop()})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestMemoryListAndSearch(t *testing.T) {
	srv := newMemoryTestServer(t, &fakeMemEngine{
		hits:   []memory.Hit{{ID: "m1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", UpdatedAt: 1}},
		listed: []memory.Hit{{ID: "m1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", UpdatedAt: 1}},
	})
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
	if body.Engine != "ladym" || len(body.Items) != 1 || body.Items[0].ID != "m1" || body.Total != 1 {
		t.Fatalf("list body = %+v", body)
	}
	// with q -> Recall
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
	if len(b2.Items) != 1 {
		t.Fatalf("search body = %+v", b2)
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
	// bad limit clamps
	resp2, err := http.Get(srv.URL + "/api/memory?limit=9999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("clamped limit status = %d", resp2.StatusCode)
	}
}
