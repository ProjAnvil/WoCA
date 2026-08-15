package console

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/server"
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
	return New(Deps{Agent: a, Cfg: cfg, Web: web, ModelFactory: factory, Version: "test"}), cfg, web
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
