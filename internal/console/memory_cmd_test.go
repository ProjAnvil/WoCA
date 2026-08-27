package console

import (
	"context"
	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string, since float64) (string, error) {
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

func TestMemoryEngineCommandSurface(t *testing.T) {
	c, eng := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	exec := func(args ...string) (string, error) {
		res, err := c.Execute(ctx, "memory", args, s)
		return res.Output, err
	}

	out, err := exec("list")
	if err != nil || !strings.Contains(out, "stored fact") {
		t.Fatalf("list = %q %v", out, err)
	}
	out, err = exec("recall", "deploy")
	if err != nil || !strings.Contains(out, "hit for deploy") {
		t.Fatalf("recall = %q %v", out, err)
	}
	out, err = exec("remember", "the answer is 42")
	if err != nil || !strings.Contains(out, "id-1") {
		t.Fatalf("remember = %q %v", out, err)
	}
	out, err = exec("remember", "drop-me please")
	if err != nil || !strings.Contains(out, "attention gate") {
		t.Fatalf("dropped = %q %v", out, err)
	}
	out, err = exec("stats")
	if err != nil || !strings.Contains(out, "total: 3") {
		t.Fatalf("stats = %q %v", out, err)
	}
	out, err = exec("forget", "h9")
	if err != nil || !strings.Contains(out, "forgotten") {
		t.Fatalf("forget = %q %v", out, err)
	}
	if len(eng.forgotten) != 1 || eng.forgotten[0] != "h9" {
		t.Fatalf("engine forget = %v", eng.forgotten)
	}
	out, err = exec("consolidate")
	if err != nil || !strings.Contains(out, "consolidated") {
		t.Fatalf("consolidate = %q %v", out, err)
	}
	out, err = exec("index", "/tmp")
	if err != nil || !strings.Contains(out, "indexed") {
		t.Fatalf("index = %q %v", out, err)
	}
	// Usage errors and unknown subcommands.
	for _, args := range [][]string{{"recall"}, {"remember"}, {"forget"}, {"nope"}} {
		if _, err := exec(args...); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
	// The kv subcommand still reaches the store under an engine.
	out, err = exec("kv", "put", "ns", "k", "value")
	if err != nil || !strings.Contains(out, "stored") {
		t.Fatalf("kv put = %q %v", out, err)
	}
	out, err = exec("kv", "get", "ns", "k")
	if err != nil || !strings.Contains(out, "value") {
		t.Fatalf("kv get = %q %v", out, err)
	}
}

func TestMemoryKVCommandSurface(t *testing.T) {
	// No engine wired: the KV store is the surface.
	c, _ := newAgentConsole(t, nil)
	c.deps.Memory = store.NewMemory()
	ctx := context.Background()
	s := &Session{}

	res, err := c.Execute(ctx, "memory", []string{"list"}, s)
	out := res.Output
	if err != nil || !strings.Contains(out, "empty") {
		t.Fatalf("empty list = %q %v", out, err)
	}
	// Put/get/del round trip with namespace listing.
	for _, args := range [][]string{
		{"put", "ns", "k1", "v1"},
		{"put", "ns2", "k2", "v2"},
	} {
		if _, err := c.Execute(ctx, "memory", args, s); err != nil {
			t.Fatalf("%v = %v", args, err)
		}
	}
	res, _ = c.Execute(ctx, "memory", []string{"list"}, s)
	out = res.Output
	if !strings.Contains(out, `ns/k1 = "v1"`) {
		t.Fatalf("list = %q", out)
	}
	res, _ = c.Execute(ctx, "memory", []string{"get", "ns", "k1"}, s)
	out = res.Output
	if !strings.Contains(out, "v1") {
		t.Fatalf("get = %q", out)
	}
	res, _ = c.Execute(ctx, "memory", []string{"get", "ns", "missing"}, s)
	out = res.Output
	if !strings.Contains(out, "not found") {
		t.Fatalf("missing get = %q", out)
	}
	if _, err := c.Execute(ctx, "memory", []string{"del", "ns", "k1"}, s); err != nil {
		t.Fatal(err)
	}
	// Usage errors.
	for _, args := range [][]string{{"get", "ns"}, {"put", "ns", "k"}, {"del", "ns"}, {"bogus"}} {
		if _, err := c.Execute(ctx, "memory", args, s); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}

	// Neither engine nor store: helpful error.
	c2, _ := newAgentConsole(t, nil)
	if _, err := c2.Execute(ctx, "memory", nil, &Session{}); err == nil {
		t.Fatal("expected disabled error")
	}
}

func TestMemoryEngineErrorPaths(t *testing.T) {
	home := t.TempDir()
	acfg := agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		MemoryEngine:   &errEngine{},
	}
	a, err := agent.New(acfg)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Agent: a, Cfg: &config.Config{Model: "m", WorkDir: home}})
	ctx := context.Background()
	s := &Session{}
	for _, args := range [][]string{
		{"list"}, {"recall", "q"}, {"remember", "x"}, {"stats"}, {"forget", "id"},
		{"consolidate"}, {"index", "/tmp"},
	} {
		if _, err := c.Execute(ctx, "memory", args, s); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
	// /memory kv without a store under an engine errors.
	if _, err := c.Execute(ctx, "memory", []string{"kv", "list"}, s); err == nil {
		t.Fatal("expected kv disabled error")
	}
	// Empty engine list output (the fake returns nothing for empty ws).
	c2 := New(Deps{
		Agent:           newAgentWithEngine(t, &fakeEngine{}),
		Cfg:             &config.Config{WorkDir: home},
		MemoryWorkspace: func() string { return "empty-ws" },
	})
	if out, err := c2.Execute(ctx, "memory", []string{"list"}, s); err != nil || !strings.Contains(out.Output, "no memories yet") {
		t.Fatalf("empty engine list = %+v %v", out, err)
	}
}

func TestMemoryKVErrorPaths(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	mem := store.NewMemory()
	c.deps.Memory = mem
	ctx := context.Background()
	s := &Session{}
	// Seed one entry, then close the backend so every op errors.
	if err := mem.Put(ctx, "ns", "k", "v"); err != nil {
		t.Fatal(err)
	}
	// A closed sqlite store makes every op error.
	home := t.TempDir()
	db, err := wocadb.Open(filepath.Join(home, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlStore := store.NewSQL(db.DB, string(db.Dialect), db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c.deps.Memory = sqlStore
	for _, args := range [][]string{
		{"list"}, {"get", "ns", "k"}, {"put", "ns", "k", "v"}, {"del", "ns", "k"},
	} {
		if _, err := c.Execute(ctx, "memory", args, s); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
}
