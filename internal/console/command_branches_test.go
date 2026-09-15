package console

// Gap tests for console command error branches: usage errors, persistence
// failures, fallback directories, and renderer edge cases that the happy-path
// suites do not reach.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/plugins"
	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
)

// newGapConsole builds a console over a settings store the test controls,
// with optional rebuild hooks, so persistence and rebuild failures can be
// injected.
func newGapConsole(t *testing.T, mutate func(*Deps)) (*Console, *settings.Store) {
	t.Helper()
	st, err := settings.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Agent:    a,
		Settings: st,
		Cfg:      &config.Config{Model: "test-model", Provider: config.ProviderAnthropic, PermissionMode: config.ModeDefault, WorkDir: t.TempDir()},
		Version:  "test",
	}
	if mutate != nil {
		mutate(&deps)
	}
	return New(deps), st
}

func TestSettingsUsageAndPersistenceErrors(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("set usage", func(t *testing.T) {
		c, _ := newGapConsole(t, nil)
		if _, err := c.Execute(ctx, "settings", []string{"set", "provider"}, s); err == nil {
			t.Fatal("expected usage error for /settings set <key>")
		}
	})

	t.Run("closed store fails secret and plain writes", func(t *testing.T) {
		c, st := newGapConsole(t, nil)
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "settings", []string{"set", "anthropic-api-key", "sk-x"}, s); err == nil {
			t.Fatal("SetSecret on closed store should fail")
		}
		if _, err := c.Execute(ctx, "settings", []string{"set", "model", "m"}, s); err == nil {
			t.Fatal("Save on closed store should fail")
		}
	})

	t.Run("rebuild failures surface inline", func(t *testing.T) {
		boom := func() error { return errBoom }
		c, _ := newGapConsole(t, func(d *Deps) { d.RebuildMemory = boom; d.RebuildModel = boom })
		res, err := c.Execute(ctx, "settings", []string{"set", settings.KeyLadyMEmbeddingAPIKey, "ek"}, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "memory engine rebuild failed") {
			t.Fatalf("embedding key output = %q", res.Output)
		}
		res, err = c.Execute(ctx, "settings", []string{"set", "anthropic-api-key", "sk-x"}, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "model rebuild failed") {
			t.Fatalf("secret model output = %q", res.Output)
		}
		res, err = c.Execute(ctx, "settings", []string{"set", settings.KeyMaxTokens, "4096"}, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "model rebuild failed") {
			t.Fatalf("max-tokens output = %q", res.Output)
		}
	})

	t.Run("model rebuild hook invoked on success", func(t *testing.T) {
		called := false
		c, _ := newGapConsole(t, func(d *Deps) { d.RebuildModel = func() error { called = true; return nil } })
		if _, err := c.Execute(ctx, "settings", []string{"set", settings.KeyMaxTokens, "2048"}, s); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("RebuildModel hook not invoked")
		}
	})
}

// emptyRecallEngine recalls nothing, exercising the no-hits branch.
type emptyRecallEngine struct{ fakeEngine }

func (e *emptyRecallEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}

func TestMemoryCommandBranches(t *testing.T) {
	ctx := t.Context()

	t.Run("kv fallback without engine or agent", func(t *testing.T) {
		c := New(Deps{Memory: store.NewMemory()})
		if _, err := c.Execute(ctx, "memory", []string{"put", "ns", "k", "v"}, &Session{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "memory", []string{"kv"}, &Session{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "memory", []string{"kv", "list", "ns"}, &Session{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("engine default subcommand and index flags", func(t *testing.T) {
		c, _ := newFullConsole(t)
		c.deps.MemoryWorkspace = func() string { return "/ws/probe" }
		if _, err := c.Execute(ctx, "memory", nil, &Session{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "memory", []string{"recall"}, &Session{}); err == nil {
			t.Fatal("recall without query should fail")
		}
		if _, err := c.Execute(ctx, "memory", []string{"index", t.TempDir(), "--force"}, &Session{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "memory", []string{"index"}, &Session{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("recall with no hits", func(t *testing.T) {
		a := newAgentWithEngine(t, &emptyRecallEngine{})
		c := New(Deps{Agent: a, MemoryEngine: &emptyRecallEngine{}})
		res, err := c.Execute(ctx, "memory", []string{"recall", "anything"}, &Session{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "no hits") {
			t.Fatalf("empty recall = %q", res.Output)
		}
	})
}

func TestAgentsCommandBranches(t *testing.T) {
	ctx := t.Context()
	s := &Session{}
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("home fallback directory", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil) // no AgentsDir → ~/.woca/agents
		res, err := c.Execute(ctx, "agents", []string{"new", "helper", "does things"}, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, filepath.Join(home, ".woca", "agents")) {
			t.Fatalf("new output = %q", res.Output)
		}
	})

	t.Run("home resolution failure falls back to session dir", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Chdir(t.TempDir()) // the fallback home is cwd-relative; keep it in temp
		c, _ := newAgentConsole(t, nil)
		if _, err := c.Execute(ctx, "agents", []string{"new", "helper2"}, s); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("usage and creation failures", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		if _, err := c.Execute(ctx, "agents", []string{"show"}, s); err == nil {
			t.Fatal("show without name should fail")
		}
		// AgentsDir under a regular file: MkdirAll fails.
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c.deps.AgentsDir = filepath.Join(blocker, "agents")
		if _, err := c.Execute(ctx, "agents", []string{"new", "nope"}, s); err == nil {
			t.Fatal("new under a blocked path should fail")
		}
	})

	t.Run("listing variants and rm filter", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		dir := t.TempDir()
		c.deps.AgentsDir = dir
		a := c.deps.Agent
		a.SetAgentDefinitions([]agents.Definition{
			{Name: "nodesc"},
			{Name: "tooly", Description: "has tools", Tools: []string{"Read", "Grep"}, Source: filepath.Join(dir, "missing.md")},
		})
		res, err := c.Execute(ctx, "agents", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "(no description)") || !strings.Contains(res.Output, "Read,Grep") {
			t.Fatalf("list variants = %q", res.Output)
		}
		// rm keeps the other definition and reports the failing remove.
		if _, err := c.Execute(ctx, "agents", []string{"rm", "tooly"}, s); err == nil {
			t.Fatal("rm of a missing source file should fail")
		}
		if _, err := c.Execute(ctx, "agents", []string{"rm", "nodesc"}, s); err == nil {
			t.Fatal("rm without a source should fail")
		}
	})
}

func TestExportAndBranchFailures(t *testing.T) {
	ctx := t.Context()
	hist := []messages.Message{
		messages.System("plumbing"),
		messages.Human("hello"),
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "t1", Name: "Bash"}}},
		messages.AI("done"),
		{Role: messages.RoleTool, ToolCallID: "t1", Content: "output"},
	}

	t.Run("transcript rendering", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		out := filepath.Join(t.TempDir(), "chat.md")
		res, err := c.Execute(ctx, "export", []string{out}, &Session{History: hist})
		if err != nil {
			t.Fatal(err)
		}
		if res.Output == "" {
			t.Fatal("export produced no output")
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "Assistant tool call") || !strings.Contains(string(b), "tool result") {
			t.Fatalf("transcript = %q", b)
		}
		if strings.Count(string(b), "plumbing") != 0 {
			t.Fatal("system prompt leaked into transcript")
		}
	})

	t.Run("write failures", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		dir := t.TempDir()
		jsonDir := filepath.Join(dir, "x.json") // path exists as a directory
		if err := os.MkdirAll(jsonDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "export", []string{jsonDir}, &Session{History: hist}); err == nil {
			t.Fatal("json export onto a directory should fail")
		}
		mdDir := filepath.Join(dir, "chat.md")
		if err := os.MkdirAll(mdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "export", []string{mdDir}, &Session{History: hist}); err == nil {
			t.Fatal("markdown export onto a directory should fail")
		}
	})

	t.Run("branch name fallback and save failure", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		s := &Session{History: hist}
		c.deps.SessionDir = "" // default sessions dir under HOME
		if _, err := c.Execute(ctx, "branch", []string{"!!!"}, s); err != nil {
			t.Fatal(err)
		}
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c2, _ := newAgentConsole(t, nil)
		c2.deps.SessionDir = filepath.Join(blocker, "sessions")
		if _, err := c2.Execute(ctx, "branch", nil, &Session{History: hist}); err == nil {
			t.Fatal("branch under a blocked path should fail")
		}
	})

	t.Run("rename defaults and failure", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		c.deps.Agent.SetTodos([]tools.Todo{{Content: "todo", Status: "in_progress"}})
		if _, err := c.Execute(ctx, "rename", []string{"named"}, &Session{History: hist}); err != nil {
			t.Fatal(err)
		}
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c2, _ := newAgentConsole(t, nil)
		c2.deps.SessionDir = filepath.Join(blocker, "sessions")
		if _, err := c2.Execute(ctx, "rename", []string{"other"}, &Session{History: hist}); err == nil {
			t.Fatal("rename under a blocked path should fail")
		}
	})
}

func TestPreviewRendering(t *testing.T) {
	if got := preview("first\nsecond", 20); got != "first" {
		t.Fatalf("multiline = %q", got)
	}
	if got := preview(strings.Repeat("a", 30), 10); got != strings.Repeat("a", 10)+"…" {
		t.Fatalf("long = %q", got)
	}
	if got := preview("   ", 10); got != "(empty)" {
		t.Fatalf("blank = %q", got)
	}
}

func TestTeamNoTeamOutput(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	res, err := c.Execute(t.Context(), "team", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no team spawned") {
		t.Fatalf("team = %q", res.Output)
	}
}

func TestModelSwitchFactoryError(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	c.deps.ModelFactory = func(name string) (language.ChatModel, error) {
		return nil, errBoom
	}
	if _, err := c.Execute(t.Context(), "model", []string{"ghost-model"}, &Session{}); err == nil {
		t.Fatal("factory failure should surface")
	}
}

func TestWebCommandLifecycle(t *testing.T) {
	ctx := t.Context()
	c, _ := newAgentConsole(t, nil)
	web := webgui.New(server.Config{Logger: zerolog.Nop()})
	c.deps.Web = web
	t.Cleanup(func() { _ = web.Stop() })

	// Start on an ephemeral port via the port-only form, then restart on a
	// different ephemeral port (covers the stop-then-start branch).
	res, err := c.Execute(ctx, "web", []string{"0"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "web gui started") {
		t.Fatalf("start = %q", res.Output)
	}
	res, err = c.Execute(ctx, "web", []string{"0"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "web gui started") {
		t.Fatalf("restart = %q", res.Output)
	}
	res, err = c.Execute(ctx, "web", []string{"status"}, &Session{})
	if err != nil || !strings.Contains(res.Output, "running at") {
		t.Fatalf("status = %q %v", res.Output, err)
	}
	res, err = c.Execute(ctx, "web", []string{"off"}, &Session{})
	if err != nil || !strings.Contains(res.Output, "stopped") {
		t.Fatalf("off = %q %v", res.Output, err)
	}
	res, err = c.Execute(ctx, "web", []string{"off"}, &Session{})
	if err != nil || !strings.Contains(res.Output, "not running") {
		t.Fatalf("double off = %q %v", res.Output, err)
	}
}

func TestDebugLogRootlessAndPersistFailure(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("empty root renders disabled marker", func(t *testing.T) {
		a, err := agent.New(agent.Config{
			Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
			Registry:       tools.NewRegistry(t.TempDir()),
			SystemPrompt:   "test",
			PermissionMode: config.ModeDefault,
			Obs:            obslog.New("", false),
		})
		if err != nil {
			t.Fatal(err)
		}
		c := New(Deps{Agent: a, Version: "test"})
		res, err := c.Execute(ctx, "debug-log", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "(disabled)") {
			t.Fatalf("status = %q", res.Output)
		}
	})

	t.Run("persist failure surfaces", func(t *testing.T) {
		a, err := agent.New(agent.Config{
			Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
			Registry:       tools.NewRegistry(t.TempDir()),
			SystemPrompt:   "test",
			PermissionMode: config.ModeDefault,
			Obs:            obslog.New(t.TempDir(), false),
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := settings.Open(filepath.Join(t.TempDir(), "s.db"))
		if err != nil {
			t.Fatal(err)
		}
		c := New(Deps{Agent: a, Settings: st, Version: "test"})
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Execute(ctx, "debug-log", []string{"on"}, s); err == nil {
			t.Fatal("persist failure should surface")
		}
	})
}

func TestOutputStylePersistFailure(t *testing.T) {
	c, st := newGapConsole(t, nil)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(t.Context(), "output-style", []string{"explanatory"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "persist failed") {
		t.Fatalf("style output = %q", res.Output)
	}
}

func TestDoctorStorageAndHooks(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("blocked session dir", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		c.deps.SessionDir = filepath.Join(blocker, "sessions")
		res, err := c.Execute(ctx, "doctor", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "session storage") || !strings.Contains(res.Output, "✗") {
			t.Fatalf("doctor = %q", res.Output)
		}
	})

	t.Run("unwritable probe", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".doctor-probe"), 0o755); err != nil {
			t.Fatal(err)
		}
		c, _ := newAgentConsole(t, nil)
		c.deps.SessionDir = dir
		res, err := c.Execute(ctx, "doctor", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "not writable") {
			t.Fatalf("doctor = %q", res.Output)
		}
	})

	t.Run("configured hooks are summarized", func(t *testing.T) {
		c, _ := newAgentConsole(t, func(acfg *agent.Config) {
			acfg.Hooks = hooks.NewRunner(hooks.Config{PreToolUse: []hooks.Entry{{
				Matcher: ".*",
				Hooks:   []hooks.Hook{{Type: "command", Command: "true"}},
			}}}, t.TempDir(), "s1")
		})
		res, err := c.Execute(ctx, "doctor", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "PreToolUse") {
			t.Fatalf("doctor hooks = %q", res.Output)
		}
	})

	t.Run("home resolution failure degrades oauth probe", func(t *testing.T) {
		t.Setenv("HOME", "")
		if oauthTokenPresent() {
			t.Fatal("oauth probe should miss without a home dir")
		}
	})
}

func TestGoalCommandGapBranches(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("budget without a goal", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		if _, err := c.Execute(ctx, "goal", []string{"budget", "cycles", "5"}, s); err == nil {
			t.Fatal("budget without goal should fail")
		}
	})
}

func TestPluginsCommandBranches(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("default root and load errors", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, ".woca", "plugins")
		if err := os.MkdirAll(filepath.Join(root, "broken"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "broken", "plugin.json"), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		c, _ := newAgentConsole(t, nil)
		res, err := c.Execute(ctx, "plugins", nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "⚠") {
			t.Fatalf("broken plugin not reported: %q", res.Output)
		}
	})

	t.Run("install and uninstall failures", func(t *testing.T) {
		root := t.TempDir()
		c, _ := newAgentConsole(t, nil)
		c.deps.PluginsRoot = root
		if _, err := c.Execute(ctx, "plugins", []string{"install", filepath.Join(root, "no-such-plugin")}, s); err == nil {
			t.Fatal("install from a missing path should fail")
		}
		if _, err := c.Execute(ctx, "plugins", []string{"rm"}, s); err == nil {
			t.Fatal("rm without a name should fail")
		}
		if _, err := c.Execute(ctx, "plugins", []string{"rm", "ghost"}, s); err != nil {
			t.Fatal("RemoveAll of an absent plugin should still succeed silently")
		}
		if _, err := c.Execute(ctx, "plugins", []string{"rm", "a/b"}, s); err == nil {
			t.Fatal("rm with a path separator should fail")
		}
	})

	t.Run("plugin with skills", func(t *testing.T) {
		root := t.TempDir()
		pdir := filepath.Join(root, "helper")
		if err := os.MkdirAll(filepath.Join(pdir, "skills", "greet"), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"name": "helper", "version": "1.0", "description": "test plugin"}`
		if err := os.WriteFile(filepath.Join(pdir, "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		skill := "---\nname: greet\ndescription: says hi\n---\nSay hi.\n"
		if err := os.WriteFile(filepath.Join(pdir, "skills", "greet", "SKILL.md"), []byte(skill), 0o644); err != nil {
			t.Fatal(err)
		}
		list, errs := plugins.Load(root)
		if len(errs) > 0 || len(list) != 1 {
			t.Fatalf("load = %v %v", list, errs)
		}
		if len(list[0].Skills) != 1 {
			t.Fatalf("skills = %v", list[0].Skills)
		}
	})
}

func TestAgentctlErrorBranches(t *testing.T) {
	ctx := t.Context()
	s := &Session{}

	t.Run("thinking budget hook failure", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		c.deps.Cfg.Provider = config.ProviderAnthropic
		c.deps.Cfg.Model = "claude-sonnet-4-5"
		c.deps.SetThinkingBudget = func(int) error { return errBoom }
		if _, err := c.Execute(ctx, "thinking", []string{"high"}, s); err == nil {
			t.Fatal("thinking hook failure should surface")
		}
	})

	t.Run("quota fetch failure", func(t *testing.T) {
		c, _ := newAgentConsole(t, nil)
		c.deps.Quota = func(ctx context.Context) (quota.Info, error) {
			return quota.Info{}, errBoom
		}
		if _, err := c.Execute(ctx, "quota", nil, s); err == nil {
			t.Fatal("quota fetch failure should surface")
		}
	})
}
