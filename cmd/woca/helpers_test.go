package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

// isolateEnv points HOME and the woca env overrides at a temp directory so
// tests never touch the developer's real ~/.woca state, and disables browser
// opening. Returns the temp home.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WOCA_DB", filepath.Join(home, "woca.db"))
	t.Setenv("WOCA_PLUGIN_ROOT", filepath.Join(home, "plugins"))
	t.Setenv("WOCA_NO_BROWSER", "1")
	return home
}

// seedSettings opens the isolated settings DB, applies key=value pairs, and
// saves. Tests use it to pin cheap defaults (e.g. memory-engine=none) before
// buildAgent/run.
func seedSettings(t *testing.T, pairs ...[2]string) {
	t.Helper()
	st, err := settings.Open(os.Getenv("WOCA_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	for _, p := range pairs {
		if err := settings.Apply(&cfg, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenAI starts an OpenAI-compatible chat-completions server that always
// replies with content, handling both plain and streaming requests. Returns
// its base URL.
func fakeOpenAI(t *testing.T, content string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(delta map[string]any, finish any) {
			chunk := map[string]any{
				"id":     "chatcmpl-1",
				"object": "chat.completion.chunk",
				"choices": []map[string]any{{
					"index":         0,
					"delta":         delta,
					"finish_reason": finish,
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		writeChunk(map[string]any{"role": "assistant", "content": content}, nil)
		writeChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// baseOpts returns options wired to an isolated environment with the OpenAI
// provider pointed at url and a temp workdir.
func baseOpts(t *testing.T, url string) options {
	t.Helper()
	return options{
		provider: "openai",
		model:    "gpt-test",
		apiKey:   "test-key",
		baseURL:  url,
		workdir:  t.TempDir(),
	}
}

func TestStripSystem(t *testing.T) {
	msgs := []messages.Message{
		messages.System("sys"),
		messages.Human("hi"),
		messages.AI("hello"),
		messages.System("sys2"),
	}
	got := stripSystem(msgs)
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "hello" {
		t.Fatalf("stripSystem = %+v", got)
	}
}

func TestTurnTokenBudgetFromEnv(t *testing.T) {
	for _, c := range []struct {
		val  string
		want int
	}{
		{"", 0},
		{"  ", 0},
		{"5000", 5000},
		{"abc", 0},
		{"-3", 0},
	} {
		t.Setenv("WOCA_TURN_TOKEN_BUDGET", c.val)
		if got := turnTokenBudgetFromEnv(); got != c.want {
			t.Errorf("turnTokenBudgetFromEnv(%q) = %d, want %d", c.val, got, c.want)
		}
	}
}

func TestReadYesNo(t *testing.T) {
	if got, _ := readYesNo(bufio.NewReader(strings.NewReader("y\n"))); got != "y" {
		t.Errorf("readYesNo y = %v", got)
	}
	if got, _ := readYesNo(bufio.NewReader(strings.NewReader("\n"))); got != "n" {
		t.Errorf("readYesNo empty = %v", got)
	}
	if got, _ := readYesNo(bufio.NewReader(strings.NewReader("Q\n"))); got != "q" {
		t.Errorf("readYesNo Q = %v", got)
	}
	if got, _ := readYesNo(bufio.NewReader(strings.NewReader(""))); got != "n" {
		t.Errorf("readYesNo EOF = %v", got)
	}
}

func TestMultiHint(t *testing.T) {
	if multiHint(true) != " (comma-separated)" || multiHint(false) != "" {
		t.Fatal("multiHint")
	}
}

func TestSkillManifest(t *testing.T) {
	got := skillManifest([]skills.Skill{{Name: "commit", Description: "commit stuff"}})
	if got != "- commit: commit stuff\n" {
		t.Fatalf("skillManifest = %q", got)
	}
}

func TestWocaRoot(t *testing.T) {
	home := isolateEnv(t)
	if got := wocaRoot(); got != filepath.Join(home, ".woca") {
		t.Fatalf("wocaRoot = %q", got)
	}
}

func TestManagedSettingsPath(t *testing.T) {
	if got := managedSettingsPath("/flag.json"); got != "/flag.json" {
		t.Fatalf("flag = %q", got)
	}
	t.Setenv("WOCA_MANAGED_SETTINGS", "/env.json")
	if got := managedSettingsPath(""); got != "/env.json" {
		t.Fatalf("env = %q", got)
	}
	os.Unsetenv("WOCA_MANAGED_SETTINGS")
	if got := managedSettingsPath(""); got != "" {
		t.Fatalf("default (no /etc file on test hosts) = %q", got)
	}
}

func TestLoadOAuthAccessToken(t *testing.T) {
	home := isolateEnv(t)
	if got := loadOAuthAccessToken(); got != "" {
		t.Fatalf("missing file = %q", got)
	}
	dir := filepath.Join(home, ".woca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadOAuthAccessToken(); got != "" {
		t.Fatalf("bad json = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"access_token":"tok-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadOAuthAccessToken(); got != "tok-1" {
		t.Fatalf("token = %q", got)
	}
}

func TestResolveKey(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := resolveKey("flag-key", "openai", st); got != "flag-key" {
		t.Fatalf("flag wins: %q", got)
	}

	t.Setenv("OPENAI_API_KEY", "env-openai")
	if got := resolveKey("", "openai", st); got != "env-openai" {
		t.Fatalf("openai env: %q", got)
	}
	os.Unsetenv("OPENAI_API_KEY")
	if err := st.SetSecret(settings.SecretOpenAIAPIKey, "db-openai"); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey("", "openai", st); got != "db-openai" {
		t.Fatalf("openai db: %q", got)
	}

	t.Setenv("GLM_API_KEY", "env-glm")
	if got := resolveKey("", "glm", st); got != "env-glm" {
		t.Fatalf("glm env: %q", got)
	}
	os.Unsetenv("GLM_API_KEY")
	if err := st.SetSecret(settings.SecretGLMAPIKey, "db-glm"); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey("", "glm", st); got != "db-glm" {
		t.Fatalf("glm db: %q", got)
	}

	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek")
	if got := resolveKey("", "deepseek", st); got != "env-deepseek" {
		t.Fatalf("deepseek env: %q", got)
	}
	os.Unsetenv("DEEPSEEK_API_KEY")
	if err := st.SetSecret(settings.SecretDeepSeekAPIKey, "db-deepseek"); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey("", "deepseek", st); got != "db-deepseek" {
		t.Fatalf("deepseek db: %q", got)
	}

	// Default provider (anthropic): env, then db secret, then OAuth token.
	t.Setenv("ANTHROPIC_API_KEY", "env-anthropic")
	if got := resolveKey("", "anthropic", st); got != "env-anthropic" {
		t.Fatalf("anthropic env: %q", got)
	}
	os.Unsetenv("ANTHROPIC_API_KEY")
	if err := st.SetSecret(settings.SecretAnthropicAPIKey, "db-anthropic"); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey("", "anthropic", st); got != "db-anthropic" {
		t.Fatalf("anthropic db: %q", got)
	}
	if err := st.SetSecret(settings.SecretAnthropicAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	// Falls back to the OAuth token file.
	dir := filepath.Join(home, ".woca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"access_token":"oauth-tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey("", "anthropic", st); got != "oauth-tok" {
		t.Fatalf("oauth fallback: %q", got)
	}
}

func TestResolveBaseURL(t *testing.T) {
	db := settings.Settings{}
	if got := resolveBaseURL("http://flag", "openai", db); got != "http://flag" {
		t.Fatalf("flag: %q", got)
	}

	t.Setenv("OPENAI_BASE_URL", "http://env-openai")
	if got := resolveBaseURL("", "openai", db); got != "http://env-openai" {
		t.Fatalf("openai env: %q", got)
	}
	os.Unsetenv("OPENAI_BASE_URL")
	db.OpenAIBaseURL = "http://db-openai"
	if got := resolveBaseURL("", "openai", db); got != "http://db-openai" {
		t.Fatalf("openai db: %q", got)
	}

	t.Setenv("GLM_BASE_URL", "http://env-glm")
	if got := resolveBaseURL("", "glm", db); got != "http://env-glm" {
		t.Fatalf("glm env: %q", got)
	}
	os.Unsetenv("GLM_BASE_URL")
	if got := resolveBaseURL("", "glm", settings.Settings{}); got != config.ProviderDefaultBaseURL(config.ProviderGLM) {
		t.Fatalf("glm default: %q", got)
	}

	t.Setenv("DEEPSEEK_BASE_URL", "http://env-ds")
	if got := resolveBaseURL("", "deepseek", db); got != "http://env-ds" {
		t.Fatalf("deepseek env: %q", got)
	}
	os.Unsetenv("DEEPSEEK_BASE_URL")
	if got := resolveBaseURL("", "deepseek", settings.Settings{}); got != config.ProviderDefaultBaseURL(config.ProviderDeepSeek) {
		t.Fatalf("deepseek default: %q", got)
	}

	t.Setenv("ANTHROPIC_BASE_URL", "http://env-an")
	if got := resolveBaseURL("", "anthropic", db); got != "http://env-an" {
		t.Fatalf("anthropic env: %q", got)
	}
	os.Unsetenv("ANTHROPIC_BASE_URL")
	t.Setenv("ANTHROPIC_API_URL", "http://apiurl-an")
	if got := resolveBaseURL("", "anthropic", db); got != "http://apiurl-an" {
		t.Fatalf("anthropic api url: %q", got)
	}
	os.Unsetenv("ANTHROPIC_API_URL")
	db.AnthropicBaseURL = "http://db-an"
	if got := resolveBaseURL("", "anthropic", db); got != "http://db-an" {
		t.Fatalf("anthropic db: %q", got)
	}
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(strings.NewReader("x")) {
		t.Fatal("string reader is not a terminal")
	}
}

func TestRunShellCommand(t *testing.T) {
	if out := runShellCommand("echo shell-ok"); !strings.Contains(out, "shell-ok") {
		t.Fatalf("echo out = %q", out)
	}
	out := runShellCommand("exit 3")
	if !strings.Contains(out, "[exit:") {
		t.Fatalf("failing out = %q", out)
	}
}

func TestPluginListOrEmpty(t *testing.T) {
	home := isolateEnv(t)
	if got := pluginListOrEmpty(); len(got) != 0 {
		t.Fatalf("empty root = %+v", got)
	}
	// A root that is a regular file makes Load fail; the list must stay empty
	// (errors swallowed).
	bad := filepath.Join(home, "plugins")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WOCA_PLUGIN_ROOT", bad)
	if got := pluginListOrEmpty(); len(got) != 0 {
		t.Fatalf("broken root = %+v", got)
	}
}

func TestSaveLastSessionAndEndSession(t *testing.T) {
	home := isolateEnv(t)
	saveLastSession(nil, nil) // early return, no file written
	if _, err := os.Stat(filepath.Join(home, ".woca", "last-session.jsonl")); !os.IsNotExist(err) {
		t.Fatal("empty session should not write a file")
	}

	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	endSession(t.Context(), []messages.Message{messages.Human("hi")}, a)
	data, err := os.ReadFile(filepath.Join(home, ".woca", "last-session.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("last-session file: %v %d bytes", err, len(data))
	}
}

func TestSubmitHooks(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	// No hooks: prompt passes through unchanged.
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := submitHooks(t.Context(), a, "hello"); got != "hello" {
		t.Fatalf("no-hook submitHooks = %q", got)
	}

	// A UserPromptSubmit hook that prints feedback prepends it.
	dir := t.TempDir()
	cfg := hooks.Config{UserPromptSubmit: []hooks.Entry{{
		Hooks: []hooks.Hook{{Type: "command", Command: "echo hook-feedback"}},
	}}}
	a2, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Hooks:          hooks.NewRunner(cfg, dir, "woca"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := submitHooks(t.Context(), a2, "hello")
	if !strings.HasPrefix(got, "hook-feedback") || !strings.HasSuffix(got, "hello") {
		t.Fatalf("hook submitHooks = %q", got)
	}
}

func TestInterruptHandler(t *testing.T) {
	var stderr bytes.Buffer
	nonInteractive := interruptHandler(bufio.NewReader(strings.NewReader("")), false, &stderr)

	// Permission, non-interactive: denied.
	v, err := nonInteractive(agent.PermissionRequest{ToolName: "Bash", Reason: "runs stuff"})
	if err != nil || v != "n" {
		t.Fatalf("non-interactive permission = %v %v", v, err)
	}

	// AskUser, non-interactive: empty answers.
	ans, err := nonInteractive(agent.AskUserRequest{Questions: []tools.Question{{Question: "q1"}, {Question: "q2", MultiSelect: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if aa, ok := ans.(agent.AskUserAnswer); !ok || len(aa.Answers) != 0 {
		t.Fatalf("non-interactive ask = %#v", ans)
	}

	// Plan approval, non-interactive: denied.
	v, err = nonInteractive(agent.PlanApprovalRequest{Plan: "do it"})
	if err != nil || v != "n" {
		t.Fatalf("non-interactive plan = %v %v", v, err)
	}

	// Unknown interrupt type.
	if _, err := nonInteractive(42); err == nil {
		t.Fatal("unknown interrupt should error")
	}

	interactive := interruptHandler(bufio.NewReader(strings.NewReader("y\n")), true, &stderr)
	v, err = interactive(agent.PermissionRequest{ToolName: "Bash", Reason: "runs stuff"})
	if err != nil || v != "y" {
		t.Fatalf("interactive permission = %v %v", v, err)
	}

	interactive = interruptHandler(bufio.NewReader(strings.NewReader("a\nb\n")), true, &stderr)
	ans, err = interactive(agent.AskUserRequest{Questions: []tools.Question{{Question: "q1"}, {Question: "q2", MultiSelect: true}}})
	if err != nil {
		t.Fatal(err)
	}
	aa := ans.(agent.AskUserAnswer)
	if len(aa.Answers) != 2 || aa.Answers[0] != "a" || aa.Answers[1] != "b" {
		t.Fatalf("interactive ask = %#v", ans)
	}

	interactive = interruptHandler(bufio.NewReader(strings.NewReader("")), true, &stderr)
	v, err = interactive(agent.PlanApprovalRequest{Plan: "the plan"})
	if err != nil || v != "n" {
		t.Fatalf("interactive plan EOF = %v %v", v, err)
	}
}

func TestLoadAgents(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".woca", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: reviewer\ndescription: reviews code\n---\nYou review code."
	if err := os.WriteFile(filepath.Join(agentsDir, "reviewer.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	list := loadAgents(dir)
	if len(list) != 1 || list[0].Name != "reviewer" {
		t.Fatalf("agents = %+v", list)
	}
}
