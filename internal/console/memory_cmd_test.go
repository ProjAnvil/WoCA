package console

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/woca/internal/memory"
)

// fakeMemEngine is an in-memory memory.Engine for /memory command tests.
type fakeMemEngine struct {
	mu         sync.Mutex
	hits       []memory.Hit
	remembered []string
	stats      *memory.Stats
}

func (f *fakeMemEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, nil
}
func (f *fakeMemEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeMemEngine) Remember(ctx context.Context, ws, c string, tags []string, src string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remembered = append(f.remembered, ws+"|"+c)
	return "m1", nil
}
func (f *fakeMemEngine) RecordEvent(ctx context.Context, ws, a, act, obs, o string) error { return nil }
func (f *fakeMemEngine) List(ctx context.Context, ws, layer string, limit, off int) ([]memory.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, nil
}
func (f *fakeMemEngine) Forget(ctx context.Context, id string) error { return nil }
func (f *fakeMemEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	if f.stats != nil {
		return f.stats, nil
	}
	return &memory.Stats{Total: 3, Layers: map[string]int{"L2_semantic": 2, "L1_episodic": 1}}, nil
}
func (f *fakeMemEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "indexed 1 of 1 files", nil
}
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	return "cycle done", nil
}
func (f *fakeMemEngine) Description() string { return "ladym fake.db" }
func (f *fakeMemEngine) Close() error        { return nil }

func TestMemoryCmdEngineSubcommands(t *testing.T) {
	c := New(Deps{
		MemoryEngine:    &fakeMemEngine{hits: []memory.Hit{{ID: "m1", Layer: "L2_semantic", Type: "fact", Summary: "JWT fact", Score: 0.8}}},
		MemoryWorkspace: func() string { return "/ws" },
	})
	sess := &Session{}
	res, err := c.Execute(context.Background(), "memory", []string{"recall", "auth"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "[0.80|L2_semantic|fact] JWT fact") {
		t.Fatalf("recall output = %q", res.Output)
	}
	res, err = c.Execute(context.Background(), "memory", []string{"stats"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "ladym fake.db") || !strings.Contains(res.Output, "L2_semantic") || !strings.Contains(res.Output, "total: 3") {
		t.Fatalf("stats output = %q", res.Output)
	}
	res, err = c.Execute(context.Background(), "memory", []string{"remember", "prefers tabs"}, sess)
	if err != nil || !strings.Contains(res.Output, "stored") {
		t.Fatalf("remember = %q, %v", res.Output, err)
	}
	res, err = c.Execute(context.Background(), "memory", []string{"consolidate"}, sess)
	if err != nil || !strings.Contains(res.Output, "cycle done") {
		t.Fatalf("consolidate = %q, %v", res.Output, err)
	}
	res, err = c.Execute(context.Background(), "memory", []string{"list"}, sess)
	if err != nil || !strings.Contains(res.Output, "JWT fact") {
		t.Fatalf("list = %q, %v", res.Output, err)
	}
}

func TestMemoryCmdEngineNilFallsBackToKVMessage(t *testing.T) {
	c := New(Deps{}) // no engine, no KV store
	_, err := c.Execute(context.Background(), "memory", []string{"list"}, &Session{})
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("expected helpful error, got %v", err)
	}
}
