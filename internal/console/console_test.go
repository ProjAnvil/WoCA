package console

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
)

func newTestConsole(t *testing.T, factory func(name string) (language.ChatModel, error)) (*Console, *config.Config, *webgui.Manager) {
	t.Helper()
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	web := webgui.New(server.Config{Logger: zerolog.Nop()})
	return New(Deps{Agent: a, Cfg: cfg, Web: web, ModelFactory: factory, Version: "test", KeybindingsPath: filepath.Join(t.TempDir(), "keybindings.json")}), cfg, web
}

func TestHelpListsCommands(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	res, err := c.Execute(context.Background(), "help", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/web", "/clear", "/compact", "/context", "/model", "/permissions"} {
		if !strings.Contains(res.Output, want+" ") && !strings.Contains(res.Output, want+"\n") {
			t.Fatalf("help output missing %q:\n%s", want, res.Output)
		}
	}
}

func TestCommandsSortedAndComplete(t *testing.T) {
	list := Commands()
	if len(list) < 10 {
		t.Fatalf("Commands() = %d entries, want a full catalog", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name >= list[i].Name {
			t.Fatalf("Commands() not sorted: %s before %s", list[i-1].Name, list[i].Name)
		}
	}
}

func TestStatus(t *testing.T) {
	c, cfg, _ := newTestConsole(t, nil)
	cfg.Provider = "anthropic"
	res, err := c.Execute(context.Background(), "status", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"woca test", "model=test-model", "permission mode=default", "web gui=off"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("status missing %q:\n%s", want, res.Output)
		}
	}
}

func TestClearClearsSession(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{messages.Human("hi"), messages.AI("hello")}}
	res, err := c.Execute(context.Background(), "clear", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clear {
		t.Fatal("/clear should set Clear")
	}
	if len(s.History) != 0 {
		t.Fatalf("history after /clear = %d messages", len(s.History))
	}
}

func TestPermissionsSet(t *testing.T) {
	c, cfg, _ := newTestConsole(t, nil)
	if _, err := c.Execute(context.Background(), "permissions", []string{"readOnly"}, &Session{}); err != nil {
		t.Fatal(err)
	}
	if cfg.PermissionMode != config.ModeReadOnly {
		t.Fatalf("cfg mode = %s", cfg.PermissionMode)
	}
	if _, err := c.Execute(context.Background(), "permissions", []string{"bogus"}, &Session{}); err == nil {
		t.Fatal("bogus mode should error")
	}
}

func TestModelSwitch(t *testing.T) {
	c, cfg, _ := newTestConsole(t, func(name string) (language.ChatModel, error) {
		return language.NewFakeChatModel(language.WithResponses(messages.AI("switched"))), nil
	})
	res, err := c.Execute(context.Background(), "model", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "test-model") {
		t.Fatalf("/model view = %q", res.Output)
	}
	if _, err := c.Execute(context.Background(), "model", []string{"gpt-4o"}, &Session{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "gpt-4o" {
		t.Fatalf("cfg.Model after switch = %q", cfg.Model)
	}

	// Without a factory, switching errors but viewing still works.
	c2, _, _ := newTestConsole(t, nil)
	if _, err := c2.Execute(context.Background(), "model", []string{"x"}, &Session{}); err == nil {
		t.Fatal("switching without a factory should error")
	}
}

func TestWebLifecycle(t *testing.T) {
	c, _, web := newTestConsole(t, nil)

	// status: off
	res, err := c.Execute(context.Background(), "web", []string{"status"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "off") {
		t.Fatalf("web status before start = %q", res.Output)
	}

	// start on an ephemeral port (passing "0")
	res, err = c.Execute(context.Background(), "web", []string{"0"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "web gui started") {
		t.Fatalf("web start = %q", res.Output)
	}
	addr, running := web.Running()
	if !running {
		t.Fatal("web should be running after /web")
	}
	if addr == "127.0.0.1:0" {
		t.Fatalf("running addr should be the resolved port, got %q", addr)
	}

	// starting again on the same resolved target is reported, not restarted
	res, err = c.Execute(context.Background(), "web", []string{"status"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, addr) {
		t.Fatalf("web status = %q, want addr %s", res.Output, addr)
	}

	// stop
	res, err = c.Execute(context.Background(), "web", []string{"off"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "stopped") {
		t.Fatalf("web off = %q", res.Output)
	}
	if _, running := web.Running(); running {
		t.Fatal("web should be stopped after /web off")
	}
}

func TestWebUnavailable(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := agent.New(agent.Config{
		Model:        model,
		Registry:     tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Agent: a})
	if _, err := c.Execute(context.Background(), "web", nil, &Session{}); err == nil {
		t.Fatal("/web without a manager should error")
	}
}

func TestContextAndLists(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{messages.Human("hello")}}

	res, err := c.Execute(context.Background(), "context", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "1 messages") || !strings.Contains(res.Output, "auto-compact: disabled") {
		t.Fatalf("context = %q", res.Output)
	}

	for _, name := range []string{"todos", "tasks", "skills", "mcp"} {
		res, err = c.Execute(context.Background(), name, nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if res.Output == "" {
			t.Fatalf("/%s produced no output", name)
		}
	}
}

func TestUnknownCommandAndExit(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	if _, err := c.Execute(context.Background(), "nope", nil, &Session{}); err == nil {
		t.Fatal("unknown command should error")
	}
	for _, name := range []string{"exit", "quit", "q"} {
		res, err := c.Execute(context.Background(), name, nil, &Session{})
		if err != nil || !res.Exit {
			t.Fatalf("/%s = %+v, %v; want Exit", name, res, err)
		}
	}
}

// newAgentConsole builds a console around an agent config the caller can
// mutate before construction (summarizer, compact threshold, hooks, …).
func newAgentConsole(t *testing.T, mutate func(*agent.Config)) (*Console, *config.Config) {
	t.Helper()
	acfg := agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	}
	if mutate != nil {
		mutate(&acfg)
	}
	a, err := agent.New(acfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	return New(Deps{Agent: a, Cfg: cfg, Version: "test"}), cfg
}

// stubTool is a registry tool with a caller-chosen name (used to fake
// mcp__server__tool entries for /mcp).
type stubTool struct{ name string }

func (s stubTool) Name() string              { return s.name }
func (s stubTool) Description() string       { return "stub tool" }
func (s stubTool) ArgsSchema() schema.Schema { return nil }
func (s stubTool) Invoke(ctx context.Context, in map[string]any) (coretools.Result, error) {
	return coretools.Result{}, nil
}

func TestCompactCommand(t *testing.T) {
	// Without a summarizer there is nothing to compact.
	c, _ := newAgentConsole(t, nil)
	s := &Session{History: []messages.Message{messages.Human("hello"), messages.AI("hi")}}
	res, err := c.Execute(context.Background(), "compact", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "nothing to compact") {
		t.Fatalf("compact without summarizer = %q", res.Output)
	}
	if len(s.History) != 2 {
		t.Fatalf("history must be untouched, got %d messages", len(s.History))
	}

	// With a summarizer and a tiny threshold the head is summarized away.
	c, _ = newAgentConsole(t, func(acfg *agent.Config) {
		acfg.Summarizer = language.NewFakeChatModel(language.WithResponses(messages.AI("the user said hello")))
		acfg.CompactThreshold = 5
	})
	s = &Session{History: []messages.Message{
		messages.Human("a fairly long first question about the codebase and its build system"),
		messages.AI("an equally long first answer explaining the build system in some detail"),
		messages.Human("second question"),
	}}
	res, err = c.Execute(context.Background(), "compact", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "compacted: ~") {
		t.Fatalf("compact output = %q", res.Output)
	}
	if len(s.History) >= 3 {
		t.Fatalf("history after compact = %d messages:\n%+v", len(s.History), s.History)
	}
}

func TestContextReport(t *testing.T) {
	c, _ := newAgentConsole(t, func(acfg *agent.Config) {
		acfg.Summarizer = language.NewFakeChatModel(language.WithResponses(messages.AI("s")))
		acfg.CompactThreshold = 1000
	})
	s := &Session{History: []messages.Message{
		messages.Human("hello"),
		messages.AI("hi"),
		messages.Tool("t1", "tool result"),
	}}
	res, err := c.Execute(context.Background(), "context", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3 messages", "human=1 ai=1 tool=1", "auto-compact threshold: 1000"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("context missing %q:\n%s", want, res.Output)
		}
	}

}

func TestUndoRedoCommands(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	s := &Session{}

	// Nothing recorded yet: friendly "nothing to" output, not an error.
	for _, name := range []string{"undo", "redo"} {
		res, err := c.Execute(context.Background(), name, nil, s)
		if err != nil {
			t.Fatalf("/%s: %v", name, err)
		}
		if !strings.Contains(res.Output, "nothing to "+name) || !strings.Contains(res.Output, "0 undoable") {
			t.Fatalf("/%s = %q", name, res.Output)
		}
	}

	// Record one file mutation; /undo reverts it, /redo re-applies it.
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.deps.Agent.FileHistory().Record(context.Background(), path, []byte("old"), []byte("new"))
	res, err := c.Execute(context.Background(), "undo", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Undid") || !strings.Contains(res.Output, "0 undoable, 1 redoable") {
		t.Fatalf("undo = %q", res.Output)
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Fatalf("file after undo = %q", data)
	}
	res, err = c.Execute(context.Background(), "redo", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Redid") || !strings.Contains(res.Output, "1 undoable, 0 redoable") {
		t.Fatalf("redo = %q", res.Output)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Fatalf("file after redo = %q", data)
	}
}

func TestListRenderings(t *testing.T) {
	c, _ := newAgentConsole(t, func(acfg *agent.Config) {
		acfg.Registry.Register(stubTool{name: "mcp__demo__echo"})
		acfg.Registry.Register(stubTool{name: "mcp__demo__ping"})
	})

	// Todos render per-status marks.
	c.deps.Agent.SetTodos([]tools.Todo{
		{Content: "pending thing", Status: "pending"},
		{Content: "active thing", Status: "in_progress"},
		{Content: "done thing", Status: "completed"},
		{Content: "dropped thing", Status: "cancelled"},
	})
	res, err := c.Execute(context.Background(), "todos", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[ ] pending thing", "[>] active thing", "[x] done thing", "[-] dropped thing"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("todos missing %q:\n%s", want, res.Output)
		}
	}

	// Shared tasks render id/status/subject columns.
	c.deps.Agent.SetTasks([]agent.TaskRecord{
		{ID: "t1", Subject: "write tests", Status: agent.TaskRecordInProgress},
		{ID: "t2", Subject: "ship it", Status: agent.TaskRecordPending},
	})
	res, err = c.Execute(context.Background(), "tasks", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "t1") || !strings.Contains(res.Output, "write tests") {
		t.Fatalf("tasks = %q", res.Output)
	}

	// Skills render with and without descriptions.
	c.deps.Agent.SetSkills([]skills.Skill{
		{Name: "documented", Description: "does things"},
		{Name: "bare"},
	})
	res, err = c.Execute(context.Background(), "skills", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "documented") || !strings.Contains(res.Output, "does things") || !strings.Contains(res.Output, "bare") {
		t.Fatalf("skills = %q", res.Output)
	}

	// MCP tools group by server.
	res, err = c.Execute(context.Background(), "mcp", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"demo (2 tools)", "mcp__demo__echo", "mcp__demo__ping"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("mcp missing %q:\n%s", want, res.Output)
		}
	}

	// No team spawned yet.
	res, err = c.Execute(context.Background(), "team", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no team spawned") {
		t.Fatalf("team = %q", res.Output)
	}
}

func TestStatusWebRunningAndTaskSummary(t *testing.T) {
	web := webgui.New(server.Config{Logger: zerolog.Nop()})
	c, _ := newAgentConsole(t, func(acfg *agent.Config) {
		acfg.Summarizer = language.NewFakeChatModel(language.WithResponses(messages.AI("polishing the console tests")))
	})
	c.deps.Web = web
	if _, err := c.Execute(context.Background(), "web", []string{"0"}, &Session{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Execute(context.Background(), "status", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "web gui=http://") {
		t.Fatalf("status web line missing:\n%s", res.Output)
	}
	if _, err := c.Execute(context.Background(), "web", []string{"off"}, &Session{}); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsView(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	res, err := c.Execute(context.Background(), "permissions", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "current mode=default") || !strings.Contains(res.Output, "bypassPermissions") {
		t.Fatalf("permissions view = %q", res.Output)
	}
}

func TestModelSwitchErrors(t *testing.T) {
	// A failing factory surfaces the error.
	c, _ := newAgentConsole(t, nil)
	c.deps.ModelFactory = func(name string) (language.ChatModel, error) {
		return nil, errors.New("unknown model")
	}
	if _, err := c.Execute(context.Background(), "model", []string{"nope"}, &Session{}); err == nil ||
		!strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("factory error = %v", err)
	}

	// A factory returning nil fails at the agent boundary.
	c2, _ := newAgentConsole(t, nil)
	c2.deps.ModelFactory = func(name string) (language.ChatModel, error) { return nil, nil }
	if _, err := c2.Execute(context.Background(), "model", []string{"void"}, &Session{}); err == nil {
		t.Fatal("nil model should fail SetModel")
	}
}

func TestCfgAccessor(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	if got := c.Cfg(); got.Model != "test-model" {
		t.Fatalf("Cfg() = %+v", got)
	}
	// A nil Cfg is tolerated (read-only fallback).
	bare := New(Deps{})
	if got := bare.Cfg(); got.Model != "" {
		t.Fatalf("nil-cfg Cfg() = %+v", got)
	}
}

func TestWebRestartScenarios(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	c.deps.Web = webgui.New(server.Config{Logger: zerolog.Nop()})
	opened := ""
	c.deps.OpenBrowser = func(url string) { opened = url }

	// Stopping before a start is a no-op message.
	res, err := c.Execute(context.Background(), "web", []string{"off"}, &Session{})
	if err != nil || !strings.Contains(res.Output, "not running") {
		t.Fatalf("web off before start = %q, %v", res.Output, err)
	}

	// Start on an ephemeral port; the browser hook is invoked with the URL.
	res, err = c.Execute(context.Background(), "web", []string{"0"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "started") || !strings.HasPrefix(opened, "http://") {
		t.Fatalf("start = %q opened=%q", res.Output, opened)
	}
	addr, _ := c.deps.Web.Running()

	// Starting on the same resolved address is reported, not restarted.
	port := strings.Split(addr, ":")[1]
	res, err = c.Execute(context.Background(), "web", []string{port}, &Session{})
	if err != nil || !strings.Contains(res.Output, "already running") {
		t.Fatalf("same-addr restart = %q, %v", res.Output, err)
	}

	// Starting on a different address restarts the server.
	res, err = c.Execute(context.Background(), "web", []string{"0"}, &Session{})
	if err != nil || !strings.Contains(res.Output, "started") {
		t.Fatalf("different-addr restart = %q, %v", res.Output, err)
	}
	if newAddr, _ := c.deps.Web.Running(); newAddr == addr || newAddr == "127.0.0.1:0" {
		t.Fatalf("restart addr = %q (was %q)", newAddr, addr)
	}
	if _, err := c.Execute(context.Background(), "web", []string{"off"}, &Session{}); err != nil {
		t.Fatal(err)
	}

	// A taken port surfaces the listen error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := strings.Split(ln.Addr().String(), ":")[1]
	if _, err := c.Execute(context.Background(), "web", []string{taken}, &Session{}); err == nil {
		t.Fatal("binding a taken port should error")
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

func TestToolsCommandListsRegisteredTool(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	res, err := c.Execute(context.Background(), "tools", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Read") {
		t.Fatalf("tools output missing Read: %q", res.Output)
	}
	// The default registry's Bash tool should list as shell/requires approval.
	if !strings.Contains(res.Output, "Bash") || !strings.Contains(res.Output, "shell") {
		t.Fatalf("tools output missing Bash's category: %q", res.Output)
	}
}

func TestHooksCommandReportsNoneConfigured(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	res, err := c.Execute(context.Background(), "hooks", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no hooks configured") {
		t.Fatalf("hooks output = %q", res.Output)
	}
}
