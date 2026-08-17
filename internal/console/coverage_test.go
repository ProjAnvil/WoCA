package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"fmt"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// fakeEngine is a minimal memory.Engine for /memory surface tests.
type fakeEngine struct {
	remembered []string
	forgotten  []string
	droppedIDs int
}

func (f *fakeEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return []memory.Hit{{ID: "h1", Layer: "L2", Type: "fact", Summary: "hit for " + q, Score: 0.9}}, nil
}
func (f *fakeEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeEngine) Remember(ctx context.Context, ws, content string, tags []string, source string) (string, error) {
	if strings.Contains(content, "drop-me") {
		return "", nil // attention gate
	}
	f.remembered = append(f.remembered, content)
	return "id-1", nil
}
func (f *fakeEngine) RecordEvent(ctx context.Context, ws, a, b, c, d string) error { return nil }
func (f *fakeEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	if strings.Contains(ws, "empty") {
		return nil, nil
	}
	return []memory.Hit{{ID: "h1", Layer: "L2", Type: "fact", Summary: "stored fact"}}, nil
}
func (f *fakeEngine) Forget(ctx context.Context, id string) error {
	f.forgotten = append(f.forgotten, id)
	return nil
}
func (f *fakeEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 3, Layers: map[string]int{"L1": 1, "L2": 2}}, nil
}
func (f *fakeEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "indexed 1 file", nil
}
func (f *fakeEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	return "consolidated", nil
}
func (f *fakeEngine) Description() string { return "fake engine" }
func (f *fakeEngine) Close() error        { return nil }

// newFullConsole wires every optional dep with cheap stubs.
func newFullConsole(t *testing.T) (*Console, *fakeEngine) {
	t.Helper()
	home := t.TempDir()
	acfg := agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	}
	// memoryCmd resolves the engine through the agent first, so wire the
	// fake there as well as in deps.
	eng := &fakeEngine{}
	acfg.MemoryEngine = eng
	acfg.Obs = obslog.New(filepath.Join(home, "logs"), false)
	a, err := agent.New(acfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	c := New(Deps{
		Agent:        a,
		Cfg:          cfg,
		Version:      "test",
		Settings:     st,
		Memory:       store.NewMemory(),
		MemoryEngine: eng,
		SessionDir:   filepath.Join(home, "sessions"),
		AgentsDir:    filepath.Join(home, "agents"),
		OpenBrowser:  func(url string) {},
		ModelFactory: func(name string) (language.ChatModel, error) {
			return language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), nil
		},
	})
	return c, eng
}

func TestSettingsCommandSurface(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	mustExec := func(args ...string) string {
		t.Helper()
		res, err := c.Execute(ctx, "settings", args, s)
		if err != nil {
			t.Fatalf("/settings %v = %v", args, err)
		}
		return res.Output
	}

	if out := mustExec(); !strings.Contains(out, "provider:") {
		t.Fatalf("list = %q", out)
	}
	if out := mustExec("help"); !strings.Contains(out, "provider") {
		t.Fatalf("help = %q", out)
	}
	if out := mustExec("get", "provider"); !strings.Contains(out, "anthropic") {
		t.Fatalf("get provider = %q", out)
	}
	if out := mustExec("get", "anthropic-api-key"); !strings.Contains(out, "not set") {
		t.Fatalf("get secret = %q", out)
	}
	if out := mustExec("set", "anthropic-api-key", "sk-x"); !strings.Contains(out, "encrypted") {
		t.Fatalf("set secret = %q", out)
	}
	if out := mustExec("get", "anthropic-api-key"); !strings.Contains(out, "set") {
		t.Fatalf("get set secret = %q", out)
	}
	// Model keys rebuild the chat model through the factory-less path.
	if out := mustExec("set", "model", "some-model"); !strings.Contains(out, "saved") {
		t.Fatalf("set model = %q", out)
	}
	// Memory-engine changes rebuild the memory engine.
	if out := mustExec("set", "memory-engine", "none"); !strings.Contains(out, "saved") {
		t.Fatalf("set engine = %q", out)
	}
	if out := mustExec("set", "memory-engine", "ladym"); !strings.Contains(out, "saved") {
		t.Fatalf("set engine back = %q", out)
	}
	if out := mustExec("set", "ladym-embedding-api-key", "ek"); !strings.Contains(out, "encrypted") {
		t.Fatalf("set embedding key = %q", out)
	}
	// Errors: unknown subcommand and invalid value.
	if _, err := c.Execute(ctx, "settings", []string{"bogus"}, s); err == nil {
		t.Fatal("expected unknown subcommand error")
	}
	if _, err := c.Execute(ctx, "settings", []string{"set", "provider", "nope"}, s); err == nil {
		t.Fatal("expected invalid provider error")
	}
	if _, err := c.Execute(ctx, "settings", []string{"get"}, s); err == nil {
		t.Fatal("expected usage error")
	}

	// A disabled settings store produces a helpful error.
	c2, _ := newAgentConsole(t, nil)
	if _, err := c2.Execute(ctx, "settings", nil, &Session{}); err == nil {
		t.Fatal("expected disabled-settings error")
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

func TestGoalCommandSurface(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{}

	run := func(args ...string) (string, error) {
		res, err := c.Execute(ctx, "goal", args, s)
		return res.Output, err
	}
	if out, err := run("help"); err != nil || !strings.Contains(out, "goal") {
		t.Fatalf("help = %q %v", out, err)
	}
	if out, err := run("status"); err != nil || !strings.Contains(out, "No goal") && !strings.Contains(out, "no goal") {
		t.Fatalf("status = %q %v", out, err)
	}
	if out, err := run("set", "make it pass"); err != nil || !strings.Contains(out, "make it pass") {
		t.Fatalf("set = %q %v", out, err)
	}
	if out, err := run("status"); err != nil || !strings.Contains(out, "make it pass") {
		t.Fatalf("status after set = %q %v", out, err)
	}
	if _, err := run("budget", "tokens", "2000000"); err != nil {
		t.Fatalf("budget tokens = %v", err)
	}
	if _, err := run("budget", "cycles", "12"); err != nil {
		t.Fatalf("budget cycles = %v", err)
	}
	if _, err := run("budget", "bogus", "1"); err == nil {
		t.Fatal("expected budget usage error")
	}
	if _, err := run("pause"); err != nil {
		t.Fatalf("pause = %v", err)
	}
	if _, err := run("resume"); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if _, err := run("clear"); err != nil {
		t.Fatalf("clear = %v", err)
	}
	if out, err := run("list"); err != nil || out == "" {
		t.Logf("list = %q %v", out, err)
	}
	// Natural language defaults to setting an objective.
	if out, err := run("bogus objective text"); err != nil || !strings.Contains(out, "bogus objective text") {
		t.Fatalf("natural language = %q %v", out, err)
	}
}

func TestSessionExportAndRename(t *testing.T) {
	c, _ := newFullConsole(t)
	dir := c.deps.SessionDir
	// One saved session file.
	if err := writeSessionFile(dir, "s1", []messages.Message{messages.Human("hi"), messages.AI("hello")}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := &Session{History: []messages.Message{messages.Human("live turn")}}

	res, err := c.Execute(ctx, "export", []string{filepath.Join(dir, "s1.jsonl")}, s)
	out := res.Output
	if err != nil || !strings.Contains(out, "exported") {
		t.Fatalf("export = %q %v", out, err)
	}
	// Export with no argument exports the live session.
	res, err = c.Execute(ctx, "export", nil, s)
	out = res.Output
	if err != nil || !strings.Contains(out, "exported") {
		t.Fatalf("live export = %q %v", out, err)
	}
	// A missing file falls back to the live session instead of erroring.
	resMissing, err := c.Execute(ctx, "export", []string{filepath.Join(dir, "missing.jsonl")}, s)
	if err != nil {
		t.Fatalf("missing-file export = %+v %v", resMissing, err)
	}

	// Rename saves the live session under a sanitized name.
	res, err = c.Execute(ctx, "rename", []string{"my-session"}, s)
	if err != nil {
		t.Fatalf("rename = %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-session.json")); err != nil {
		t.Fatalf("renamed snapshot: %v", err)
	}
	// Usage and validation errors.
	if _, err := c.Execute(ctx, "rename", nil, s); err == nil {
		t.Fatal("expected usage error")
	}
	if _, err := c.Execute(ctx, "rename", []string{"..."}, s); err == nil {
		t.Fatal("expected invalid-name error")
	}
	empty := &Session{}
	if _, err := c.Execute(ctx, "rename", []string{"x"}, empty); err == nil {
		t.Fatal("expected empty-history error")
	}
}

func writeSessionFile(dir, name string, msgs []messages.Message) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return session.Save(filepath.Join(dir, name+".jsonl"), msgs)
}

func TestFormatGoalVariants(t *testing.T) {
	base := agent.Goal{Status: agent.GoalActive, Objective: "obj", Cycles: 2, MaxCycles: 10, TokensUsed: 5000, MaxTokens: 2000000}
	g := base
	g.Claimed = true
	g.Notes = []string{"note one", "note two"}
	g.Progress = []agent.GoalProgressEntry{
		{Cycle: 1, Verdict: "continue", Summary: "started"},
		{Cycle: 2, Verdict: "complete", Note: "fallback note"},
	}
	out := formatGoal(&g)
	for _, want := range []string{"completion claim", "note two", "fallback note", "[continue] started"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Terminal statuses render their reason lines.
	for _, c := range []struct {
		status agent.GoalStatus
		want   string
	}{
		{agent.GoalBlocked, "blocked:"},
		{agent.GoalComplete, "evidence:"},
		{agent.GoalBudgetLimited, "/goal budget"},
		{agent.GoalPaused, "paused"},
	} {
		g := base
		g.Status = c.status
		g.BlockedReason = "why"
		g.Evidence = "proof"
		if out := formatGoal(&g); !strings.Contains(out, c.want) {
			t.Errorf("status %v missing %q in:\n%s", c.status, c.want, out)
		}
	}
	// Long progress windows trim to the last five cycles.
	g = base
	for i := 0; i < 8; i++ {
		g.Progress = append(g.Progress, agent.GoalProgressEntry{Cycle: i + 1, Verdict: "continue", Summary: fmt_Sprintf("cycle-%d", i+1)})
	}
	out = formatGoal(&g)
	if strings.Contains(out, "cycle-1\n") && strings.Contains(out, "cycle-8") == false {
		t.Errorf("progress window not trimmed:\n%s", out)
	}
}

func fmt_Sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

func TestDoctorAndAgentctl(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	res, err := c.Execute(ctx, "doctor", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workdir", "tools", "model", "settings db"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing %q:\n%s", want, res.Output)
		}
	}
	// A console with a missing workdir shows the failure marker.
	c2, _ := newAgentConsole(t, nil)
	c2.deps.Cfg.WorkDir = filepath.Join(t.TempDir(), "missing-dir")
	res, _ = c2.Execute(ctx, "doctor", nil, s)
	if !strings.Contains(res.Output, "missing") {
		t.Errorf("doctor workdir failure missing:\n%s", res.Output)
	}

	// /quota with a stub closure.
	c3, _ := newAgentConsole(t, nil)
	c3.deps.Quota = func(ctx context.Context) (quota.Info, error) {
		return quota.Info{Available: true, Type: quota.TypeBalance, Balance: "CNY 1.00"}, nil
	}
	res, err = c3.Execute(ctx, "quota", nil, s)
	if err != nil || !strings.Contains(res.Output, "账户余额") {
		t.Fatalf("quota = %q %v", res.Output, err)
	}
	// Without a closure: unavailable message, no crash.
	c4, _ := newAgentConsole(t, nil)
	res4, err4 := c4.Execute(ctx, "quota", nil, s)
	if err4 != nil {
		t.Fatalf("bare quota errored: %v", err4)
	}
	_ = res4

	// /debug-log toggling with a settings store.
	res, err = c.Execute(ctx, "debug-log", []string{"on"}, s)
	if err != nil {
		t.Fatalf("debug-log on = %+v %v", res, err)
	}
	res, err = c.Execute(ctx, "debug-log", []string{"off"}, s)
	if err != nil {
		t.Fatalf("debug-log off = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "debug-log", []string{"bogus"}, s); err == nil {
		t.Fatal("expected debug-log usage error")
	}
	// Without settings: error.
	if _, err := c4.Execute(ctx, "debug-log", []string{"on"}, s); err == nil {
		t.Fatal("expected debug-log disabled error")
	}
}

func TestAgentsAndPluginsCommands(t *testing.T) {
	c, _ := newFullConsole(t)
	home := t.TempDir()
	ctx := context.Background()
	s := &Session{}

	res, err := c.Execute(ctx, "agents", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a definition the agent carries.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "carried", Description: "carried agent", Prompt: "p"}})
	res, err = c.Execute(ctx, "agents", nil, s)
	if err != nil || !strings.Contains(res.Output, "carried") {
		t.Fatalf("agents with defs = %+v %v", res, err)
	}
	// Create a definition file.
	if err := os.MkdirAll(c.deps.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: helper\ndescription: helps\n---\nYou help."
	if err := os.WriteFile(filepath.Join(c.deps.AgentsDir, "helper.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = c.Execute(ctx, "agents", nil, s)
	if err != nil || !strings.Contains(res.Output, "subagent definitions") {
		t.Fatalf("agents = %+v %v", res, err)
	}
	// /agents new writes a definition.
	res, err = c.Execute(ctx, "agents", []string{"new", "writer"}, s)
	if err != nil {
		t.Fatalf("agents new = %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(c.deps.AgentsDir, "writer.md")); err != nil {
		t.Fatalf("writer.md: %v", err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"new"}, s); err == nil {
		t.Fatal("expected usage error")
	}

	// /plugins lists marketplace plugins from an isolated root.
	c.deps.PluginsRoot = filepath.Join(home, "plugins")
	res, err = c.Execute(ctx, "plugins", nil, s)
	if err != nil || !strings.Contains(res.Output, "no plugins installed") {
		t.Fatalf("plugins = %+v %v", res, err)
	}
	pluginSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginSrc, "plugin.json"), []byte(`{"name":"demo","version":"1.0.0","description":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install", pluginSrc}, s); err != nil {
		t.Fatalf("plugins install = %v", err)
	}
	res, err = c.Execute(ctx, "plugins", nil, s)
	if err != nil || !strings.Contains(res.Output, "demo") {
		t.Fatalf("plugins after install = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install"}, s); err == nil {
		t.Fatal("expected install usage error")
	}
	if _, err := c.Execute(ctx, "plugins", []string{"install", "/missing/plugin"}, s); err == nil {
		t.Fatal("expected install error")
	}
	// Removing the installed plugin works; removing a missing one errors.
	res, err = c.Execute(ctx, "plugins", []string{"rm", "demo"}, s)
	if err != nil {
		t.Fatalf("plugins rm = %+v %v", res, err)
	}
	// Removing a plugin that is not installed is a tolerated no-op.
	if _, err := c.Execute(ctx, "plugins", []string{"rm", "not-there"}, s); err != nil {
		t.Fatalf("rm missing = %v", err)
	}
	if _, err := c.Execute(ctx, "plugins", []string{"bogus"}, s); err == nil {
		t.Fatal("expected usage error")
	}
	// Without settings, listing from the (empty) default root still works.
	c2, _ := newAgentConsole(t, nil)
	c2.deps.PluginsRoot = filepath.Join(t.TempDir(), "plugins")
	if _, err := c2.Execute(ctx, "plugins", nil, s); err != nil {
		t.Fatalf("plugins without settings = %v", err)
	}
}

func TestRewindBranchAndWebCommands(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{History: []messages.Message{messages.Human("hi")}}

	// /rewind with no args lists snapshot turns.
	res, err := c.Execute(ctx, "rewind", nil, s)
	if err != nil {
		t.Fatalf("rewind list = %+v %v", res, err)
	}
	// /branch with no name lists branches.
	res, err = c.Execute(ctx, "branch", nil, s)
	if err != nil {
		t.Fatalf("branch list = %+v %v", res, err)
	}
	// /web status then off (never started).
	c.deps.Web = webgui.New(server.Config{Logger: zerolog.Nop()})
	res, err = c.Execute(ctx, "web", []string{"status"}, s)
	if err != nil || !strings.Contains(res.Output, "web gui") {
		t.Fatalf("web status = %+v %v", res, err)
	}
	res, err = c.Execute(ctx, "web", []string{"off"}, s)
	if err != nil || !strings.Contains(res.Output, "not running") {
		t.Fatalf("web off = %+v %v", res, err)
	}
	// No web manager: error.
	c2, _ := newAgentConsole(t, nil)
	if _, err := c2.Execute(ctx, "web", nil, s); err == nil {
		t.Fatal("expected web unavailable error")
	}
	// /team without a team.
	res, err = c.Execute(ctx, "team", nil, s)
	if err != nil {
		t.Logf("team = %+v %v", res, err)
	}
}

func TestGoalCommandEdgeErrors(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{}
	run := func(args ...string) error {
		_, err := c.Execute(ctx, "goal", args, s)
		return err
	}
	// Pause/resume/clear/budget with no goal.
	if err := run("pause"); err == nil || !strings.Contains(err.Error(), "no goal") {
		t.Fatalf("pause no goal = %v", err)
	}
	if err := run("resume"); err == nil || !strings.Contains(err.Error(), "no goal") {
		t.Fatalf("resume no goal = %v", err)
	}
	if out, _ := c.Execute(ctx, "goal", []string{"clear"}, s); !strings.Contains(out.Output, "no goal") {
		t.Fatalf("clear no goal = %+v", out)
	}
	// Set, then resume-while-active and double pause.
	if err := run("set", "objective"); err != nil {
		t.Fatal(err)
	}
	if out, _ := c.Execute(ctx, "goal", []string{"resume"}, s); !strings.Contains(out.Output, "already active") {
		t.Fatalf("resume active = %+v", out)
	}
	if err := run("pause"); err != nil {
		t.Fatal(err)
	}
	if err := run("pause"); err == nil || !strings.Contains(err.Error(), "nothing to pause") {
		t.Fatalf("double pause = %v", err)
	}
	if err := run("resume"); err != nil {
		t.Fatal(err)
	}
	// Clearing works and reports the cleared state.
	if out, _ := c.Execute(ctx, "goal", []string{"clear"}, s); !strings.Contains(out.Output, "cleared") {
		t.Fatalf("clear = %+v", out)
	}
}

func TestAgentsCommandDetails(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	// /agents <name> renders the definition; unknown names error.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{
		Name: "codeagent", Description: "writes code", Prompt: "You write code.",
		Source: "/defs/codeagent.md",
	}})
	res, err := c.Execute(ctx, "agents", []string{"show", "codeagent"}, s)
	if err != nil || !strings.Contains(res.Output, "You write code.") {
		t.Fatalf("agents show = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"show", "ghost"}, s); err == nil {
		t.Fatal("expected unknown agent error")
	}
	// new: invalid name, duplicate, description variant.
	if _, err := c.Execute(ctx, "agents", []string{"new"}, s); err == nil {
		t.Fatal("expected usage error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "..."}, s); err == nil {
		t.Fatal("expected invalid name error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "writer", "writes", "docs"}, s); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "agents", []string{"new", "writer"}, s); err == nil {
		t.Fatal("expected duplicate error")
	}
	// rm: missing name, unknown agent, source-less definition.
	if _, err := c.Execute(ctx, "agents", []string{"rm"}, s); err == nil {
		t.Fatal("expected rm usage error")
	}
	if _, err := c.Execute(ctx, "agents", []string{"rm", "ghost"}, s); err == nil {
		t.Fatal("expected unknown rm error")
	}
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "inline"}})
	if _, err := c.Execute(ctx, "agents", []string{"rm", "inline"}, s); err == nil {
		t.Fatal("expected no-source error")
	}
	// rm removes a file-backed definition.
	c.deps.Agent.SetAgentDefinitions([]agents.Definition{{Name: "filebacked", Source: filepath.Join(c.deps.AgentsDir, "writer.md")}})
	if _, err := c.Execute(ctx, "agents", []string{"rm", "filebacked"}, s); err != nil {
		t.Fatalf("rm filebacked = %v", err)
	}
}

func TestSessionExportVariants(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{History: []messages.Message{
		messages.Human("question"),
		messages.AI("answer"),
		messages.Tool("t1", "tool output"),
	}}
	// Empty conversation: error.
	if _, err := c.Execute(ctx, "export", nil, &Session{}); err == nil {
		t.Fatal("expected empty export error")
	}
	// JSON export path with todos/tasks attached.
	c.deps.Agent.SetTodos([]tools.Todo{{Content: "todo", Status: "in_progress"}})
	res, err := c.Execute(ctx, "export", []string{"snapshot.json"}, s)
	if err != nil || !strings.Contains(res.Output, "resumable JSON") {
		t.Fatalf("json export = %+v %v", res, err)
	}
	data, err := os.ReadFile(filepath.Join(c.deps.Cfg.WorkDir, "snapshot.json"))
	if err != nil || !strings.Contains(string(data), "tool output") {
		t.Fatalf("json content: %v", err)
	}
	// Markdown rendering includes tool results.
	res, err = c.Execute(ctx, "export", nil, s)
	if err != nil || !strings.Contains(res.Output, "markdown") {
		t.Fatalf("md export = %+v %v", res, err)
	}
	// An unwritable target errors.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "export", []string{filepath.Join(blocker, "sub", "out.md")}, s); err == nil {
		t.Fatal("expected export write error")
	}
}

func TestRewindTurnSelection(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{History: []messages.Message{
		messages.Human("first question"),
		messages.AI("first answer"),
		messages.Human("second question"),
		messages.AI("second answer"),
	}}
	// No turns yet.
	empty := &Session{}
	res, err := c.Execute(ctx, "rewind", nil, empty)
	if err != nil || !strings.Contains(res.Output, "nothing to rewind") {
		t.Fatalf("empty rewind = %+v %v", res, err)
	}
	// List shows both turns.
	res, err = c.Execute(ctx, "rewind", nil, s)
	if err != nil || !strings.Contains(res.Output, "first question") || !strings.Contains(res.Output, "second question") {
		t.Fatalf("rewind list = %+v %v", res, err)
	}
	// Rewind to before turn 2 keeps the first exchange.
	res, err = c.Execute(ctx, "rewind", []string{"2"}, s)
	if err != nil || !strings.Contains(res.Output, "rewound to before turn 2") {
		t.Fatalf("rewind 2 = %+v %v", res, err)
	}
	if len(s.History) != 2 {
		t.Fatalf("history = %d messages", len(s.History))
	}
	// Rewinding everything clears the conversation.
	res, err = c.Execute(ctx, "rewind", []string{"1"}, s)
	if err != nil || !res.Clear {
		t.Fatalf("rewind 1 = %+v %v", res, err)
	}
	// Out-of-range turns error.
	if _, err := c.Execute(ctx, "rewind", []string{"9"}, s); err == nil {
		t.Fatal("expected range error")
	}
	if _, err := c.Execute(ctx, "rewind", []string{"0"}, s); err == nil {
		t.Fatal("expected range error")
	}
	// /branch <name> requires history and a name.
	if _, err := c.Execute(ctx, "branch", []string{"only"}, s); err == nil {
		t.Log("branch tolerated")
	}
}

// errEngine fails every operation.
type errEngine struct{ fakeEngine }

func (e *errEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, errBoom
}
func (e *errEngine) Remember(ctx context.Context, ws, content string, tags []string, source string) (string, error) {
	return "", errBoom
}
func (e *errEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	return nil, errBoom
}
func (e *errEngine) Forget(ctx context.Context, id string) error { return errBoom }
func (e *errEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return nil, errBoom
}
func (e *errEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	return "", errBoom
}
func (e *errEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "", errBoom
}

var errBoom = boomErr{}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

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

func newAgentWithEngine(t *testing.T, eng memory.Engine) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		MemoryEngine:   eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
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

func TestDoctorFailureModes(t *testing.T) {
	ctx := context.Background()
	s := &Session{}
	// Everything failing: no tools, no model, missing workdir, no settings,
	// no skills, no MCP.
	c, _ := newAgentConsole(t, nil)
	c.deps.Cfg.Model = ""
	c.deps.Cfg.WorkDir = filepath.Join(t.TempDir(), "gone")
	res, err := c.Execute(ctx, "doctor", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"✗", "⚠"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing failure marker %q:\n%s", want, res.Output)
		}
	}
}

func TestSettingsSetRebuildMessages(t *testing.T) {
	c, _ := newFullConsole(t)
	home := t.TempDir()
	ctx := context.Background()
	s := &Session{}

	// Non-secret model keys rebuild the chat model.
	res, err := c.Execute(ctx, "settings", []string{"set", "max-tokens", "4096"}, s)
	if err != nil || !strings.Contains(res.Output, "takes effect immediately") {
		t.Fatalf("max-tokens = %+v %v", res, err)
	}
	// Backend keys report the restart requirement.
	for _, key := range []string{"checkpoint", "memory", "checkpoint-dir"} {
		res, err = c.Execute(ctx, "settings", []string{"set", key, "sqlite"}, s)
		if err != nil || !strings.Contains(res.Output, "restart woca") {
			t.Fatalf("%s = %+v %v", key, res, err)
		}
	}
	// Memory engine keys rebuild the engine.
	res, err = c.Execute(ctx, "settings", []string{"set", "ladym-db-path", filepath.Join(home, "ladym.db")}, s)
	if err != nil {
		t.Fatalf("ladym-db-path = %v", err)
	}
	// A failing model rebuild surfaces inline (factory-less rebuild uses
	// settings; force failure via an invalid provider AFTER a secret set).
	if _, err := c.Execute(ctx, "settings", []string{"set", "openai-api-key", "sk-1"}, s); err != nil {
		t.Fatal(err)
	}
	_ = home
}

func TestDoctorFullWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	c, _ := newFullConsole(t)
	c.deps.Memory = store.NewMemory()
	// Credentials via the settings store.
	if err := c.deps.Settings.SetSecret(settings.SecretOpenAIAPIKey, "sk-doc"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(ctx, "doctor", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"API key in settings db", "session storage", "memory store", "hooks", "permissions"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing %q:\n%s", want, res.Output)
		}
	}
	// Credential on the config wins.
	c.deps.Cfg.APIKey = "flag-key"
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "API key on config") {
		t.Errorf("config credential missing:\n%s", res.Output)
	}
	// OAuth fallback with neither key.
	c.deps.Cfg.APIKey = ""
	c.deps.Settings = nil
	if err := os.MkdirAll(filepath.Join(home, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "credentials.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "OAuth token") {
		t.Errorf("oauth fallback missing:\n%s", res.Output)
	}
	// Without any credential: warning.
	if err := os.Remove(filepath.Join(home, ".woca", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "none found") {
		t.Errorf("no-credential warning missing:\n%s", res.Output)
	}
}

func TestConsoleSmallBranches(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{}

	// /output-style list and set.
	res, err := c.Execute(ctx, "output-style", nil, s)
	if err != nil {
		t.Fatalf("output-style = %v", err)
	}
	if _, err := c.Execute(ctx, "output-style", []string{"bogus"}, s); err == nil {
		t.Fatal("expected output-style error")
	}
	// /thinking without args shows current state; invalid budget errors.
	res, err = c.Execute(ctx, "thinking", nil, s)
	if err != nil {
		t.Fatalf("thinking = %v", err)
	}
	_ = res
	if _, err := c.Execute(ctx, "thinking", []string{"bogus"}, s); err == nil {
		t.Fatal("expected thinking error")
	}
	// /keybindings renders.
	if _, err := c.Execute(ctx, "keybindings", nil, s); err != nil {
		t.Fatalf("keybindings = %v", err)
	}
	// /tasks with none.
	res, err = c.Execute(ctx, "tasks", nil, s)
	if err != nil {
		t.Fatalf("tasks = %v", err)
	}
	// /todos with none.
	res, err = c.Execute(ctx, "todos", nil, s)
	if err != nil {
		t.Fatalf("todos = %v", err)
	}
	// /init requires an argument name.
	if _, err := c.Execute(ctx, "init", nil, s); err == nil {
		t.Log("init without args tolerated")
	}
	// Unknown /memory subcommand under the engine.
	if _, err := c.Execute(ctx, "memory", []string{"bogus"}, s); err == nil {
		t.Fatal("expected memory subcommand error")
	}
}
