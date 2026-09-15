package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
)

func TestInitReturnsPrompt(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{}
	res, err := c.Execute(t.Context(), "init", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompt == "" {
		t.Fatal("/init must return a prompt for the driver to submit")
	}
	if !strings.Contains(res.Prompt, "AGENTS.md") {
		t.Fatalf("prompt should target AGENTS.md: %q", res.Prompt)
	}
	if len(s.History) != 0 {
		t.Fatal("/init must not modify history itself")
	}
}

func TestInitPrefersExistingClaudeMD(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	dir := c.deps.Cfg.WorkDir
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(t.Context(), "init", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Prompt, "CLAUDE.md") || strings.Contains(res.Prompt, "AGENTS.md") {
		t.Fatalf("prompt should update the existing CLAUDE.md: %q", res.Prompt)
	}
}

// newTestConsoleWithModel builds a console around a caller-supplied model.
func newTestConsoleWithModel(t *testing.T, m language.ChatModel) (*Console, *config.Config, *webgui.Manager) {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model:          m,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	web := webgui.New(server.Config{Logger: zerolog.Nop()})
	return New(Deps{Agent: a, Cfg: cfg, Web: web, Version: "test"}), cfg, web
}

// newUsageConsole builds a console whose agent records real usage metadata
// from a fake model, then runs one conversation turn.
func newUsageConsole(t *testing.T, um messages.UsageMetadata, model string) *Console {
	t.Helper()
	resp := messages.AI("ok")
	resp.UsageMetadata = um
	fake := language.NewFakeChatModel(language.WithResponses(resp))
	c, cfg, _ := newTestConsoleWithModel(t, fake)
	cfg.Model = model
	if _, err := c.deps.Agent.Invoke(t.Context(), nil, "t"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCostEmptyAndAccumulated(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	res, err := c.Execute(t.Context(), "cost", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "no model usage") {
		t.Fatalf("empty cost = %q", res.Output)
	}

	c = newUsageConsole(t, messages.UsageMetadata{
		InputTokens:  100000,
		OutputTokens: 5000,
		TotalTokens:  105000,
		InputTokenDetails: &messages.InputTokenDetails{
			CacheReadInputTokens: 60000,
		},
	}, "claude-sonnet-4-5")
	res, err = c.Execute(t.Context(), "cost", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"requests: 1", "input:    100000 tokens", "output:   5000", "cache read 60000", "estimated cost"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("cost output missing %q:\n%s", want, res.Output)
		}
	}
	// (40k fresh + 60k×0.1 cache read)×$3/M + 5k×$15/M = $0.12+$0.018+$0.075 ≈ $0.21
	if !strings.Contains(res.Output, "$0.21") {
		t.Fatalf("sonnet estimate wrong:\n%s", res.Output)
	}
}

func TestCostUnknownModel(t *testing.T) {
	c := newUsageConsole(t, messages.UsageMetadata{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, "mystery-model")
	res, err := c.Execute(t.Context(), "cost", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "unknown") {
		t.Fatalf("unknown-model cost = %q", res.Output)
	}
}

func TestCostSurvivesClear(t *testing.T) {
	c := newUsageConsole(t, messages.UsageMetadata{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, "claude-sonnet-4-5")
	if _, err := c.Execute(t.Context(), "clear", nil, &Session{History: []messages.Message{messages.Human("x")}}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(t.Context(), "cost", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "requests: 1") {
		t.Fatalf("usage lost after /clear:\n%s", res.Output)
	}
}

func TestRewindListsAndTruncates(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{
		messages.Human("first question"),
		messages.AI("first answer"),
		messages.Tool("t1", "tool result"),
		messages.AI("more"),
		messages.Human("second question"),
		messages.AI("second answer"),
	}}

	res, err := c.Execute(t.Context(), "rewind", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "1. first question") || !strings.Contains(res.Output, "2. second question") {
		t.Fatalf("rewind list = %q", res.Output)
	}

	res, err = c.Execute(t.Context(), "rewind", []string{"2"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "rewound to before turn 2") {
		t.Fatalf("rewind result = %q", res.Output)
	}
	if len(s.History) != 4 || s.History[0].Content != "first question" {
		t.Fatalf("history after rewind = %+v", s.History)
	}

	// Out of range is an error; tagged attachments don't count as turns.
	s.History = append(s.History, messages.Human("<queued-user-input>\nx\n</queued-user-input>"))
	if _, err := c.Execute(t.Context(), "rewind", []string{"3"}, s); err == nil {
		t.Fatal("rewind 3 should fail (only 2 real turns)")
	}
	res, _ = c.Execute(t.Context(), "rewind", nil, s)
	if strings.Contains(res.Output, "queued-user-input") {
		t.Fatalf("tagged attachments must not be listed:\n%s", res.Output)
	}
}

func TestRewindToStartClears(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	s := &Session{History: []messages.Message{messages.Human("only"), messages.AI("reply")}}
	res, err := c.Execute(t.Context(), "rewind", []string{"1"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.History) != 0 || !res.Clear {
		t.Fatalf("rewind to start: history=%d clear=%v", len(s.History), res.Clear)
	}
}

func TestBranchSavesResumableSession(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	c.deps.SessionDir = filepath.Join(t.TempDir(), "sessions")
	s := &Session{History: []messages.Message{messages.Human("hello"), messages.AI("hi")}}
	res, err := c.Execute(t.Context(), "branch", []string{"experiment"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "--resume") {
		t.Fatalf("branch output = %q", res.Output)
	}
	entries, err := os.ReadDir(c.deps.SessionDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("branch files in %s: %v", c.deps.SessionDir, err)
	}
	st, err := session.LoadState(filepath.Join(c.deps.SessionDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Messages) != 2 || st.Messages[0].Content != "hello" || st.Messages[1].Content != "hi" {
		t.Fatalf("branch content = %+v", st.Messages)
	}
	if len(s.History) != 2 {
		t.Fatal("branch must not modify the live history")
	}
}

func TestBranchEmptyFails(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	c.deps.SessionDir = filepath.Join(t.TempDir(), "sessions")
	if _, err := c.Execute(t.Context(), "branch", nil, &Session{}); err == nil {
		t.Fatal("branch on an empty conversation should fail")
	}
}
