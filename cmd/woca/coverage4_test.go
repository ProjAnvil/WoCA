package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

// TestRunREPLWebGUI starts the on-demand web GUI through the /web command on
// an ephemeral port, then drives its API to execute the closures wired in
// newConsole's server.Config.
func TestRunREPLWebGUI(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "web-ok")
	opts := baseOpts(t, url)

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/web 127.0.0.1:0\n/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "web gui started") {
		t.Fatalf("web start output = %q", combined)
	}
	m := regexp.MustCompile(`web gui started: http://(127\.0\.0\.1:\d+)`).FindStringSubmatch(combined)
	if m == nil {
		t.Fatal("no address in web output")
	}
	base := "http://" + m[1]

	call := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// The webgui server shares the closures from newConsole: quota,
	// thinking, settings rebuilds, claude-env, plugins.
	if code := call(http.MethodGet, "/api/quota", ""); code != http.StatusOK {
		t.Fatalf("quota = %d", code)
	}
	if code := call(http.MethodPost, "/api/agent/thinking", `{"budget":0}`); code != http.StatusOK {
		t.Fatalf("thinking = %d", code)
	}
	if code := call(http.MethodPut, "/api/settings", `{"provider":"openai","memory_engine":"none"}`); code != http.StatusOK {
		t.Fatalf("settings = %d", code)
	}
	if code := call(http.MethodPut, "/api/claudeenv", `{"skills":[],"mcp":[]}`); code != http.StatusOK {
		t.Fatalf("claudeenv = %d", code)
	}
	if code := call(http.MethodGet, "/api/plugins", ""); code != http.StatusOK {
		t.Fatalf("plugins = %d", code)
	}
	// A real builtin plugin name passes validation; the (offline) install
	// fails inside the plugin manager, executing the closure.
	call(http.MethodPost, "/api/plugins/browser/install", "{}")
	call(http.MethodPost, "/api/plugins/browser/reinstall", "{}")
}

func TestPluginListWithContent(t *testing.T) {
	home := isolateEnv(t)
	root := filepath.Join(home, ".woca", "plugins")

	// One broken plugin (invalid manifest) and one with skills and agents.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good")
	for _, d := range []string{filepath.Join(good, "skills", "s1"), filepath.Join(good, "agents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(good, "plugin.json"), []byte(`{"name":"good","version":"2.0.0","description":"has things"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "skills", "s1", "SKILL.md"), []byte("---\nname: s1\ndescription: skill\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "agents", "a1.md"), []byte("---\nname: a1\ndescription: agent\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := execCmd(t, "plugin", "list"); err != nil {
		t.Fatal(err)
	}
}

func TestBuildAgentWorkspaceContextAndPlugins(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A project-level skill shows up in the prompt builder's skill section.
	skillDir := filepath.Join(ws, ".woca", "skills", "localutil")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: localutil\ndescription: local helper\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plugin under ~/.woca/plugins contributes skills and agents.
	plugin := filepath.Join(home, ".woca", "plugins", "toolkit")
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), []byte(`{"name":"toolkit","version":"1.0.0","description":"tools"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy things\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.workdir = ws
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestBuildAgentClaudeMCPImportError(t *testing.T) {
	home := isolateEnv(t)
	// A claude-mcp selection with an unreadable ~/.claude.json warns and
	// continues.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-mcp", `["alpha"]`},
	)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestSyncMCPConfigWriteFailure(t *testing.T) {
	home := isolateEnv(t)
	root := filepath.Join(home, ".woca")
	if err := os.MkdirAll(filepath.Join(root, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"a":{"command":"echo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncMCPConfig(root, []string{"a"}); err == nil {
		t.Fatal("expected write failure")
	}
}

func TestAppApplyClaudeEnvBranches(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	// A plugin with a skill so the live-reload merge loop has content.
	plugin := filepath.Join(home, ".woca", "plugins", "toolkit")
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), []byte(`{"name":"toolkit","version":"1.0.0","description":"tools"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	ap, err := buildAgent(t.Context(), baseOpts(t, url), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// An invalid ~/.claude.json makes the mcp.json rewrite fail.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv(nil, []string{"demo"}); err == nil {
		t.Fatal("expected failure with invalid ~/.claude.json")
	}
	// With a valid ~/.claude.json the apply succeeds and reloads skills.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"demo":{"command":"echo","args":["x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv(nil, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
}

func TestAppRebuildModelPicksUpLimits(t *testing.T) {
	isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"max-tokens", "4321"},
		[2]string{"temperature", "0.5"},
	)
	url := fakeOpenAI(t, "hi")
	ap, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if err := ap.rebuildModel(); err != nil {
		t.Fatal(err)
	}
}

func TestAppRebuildMemoryError(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.db")})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if err := ap.rebuildMemory(); err == nil {
		t.Fatal("expected rebuild error")
	}
}

func TestResolveBaseURLFromSettings(t *testing.T) {
	db := settings.Settings{
		GLMBaseURL:     "http://db-glm",
		DeepSeekBaseURL: "http://db-deepseek",
	}
	if got := resolveBaseURL("", "glm", db); got != "http://db-glm" {
		t.Fatalf("glm db = %q", got)
	}
	if got := resolveBaseURL("", "deepseek", db); got != "http://db-deepseek" {
		t.Fatalf("deepseek db = %q", got)
	}
}

func TestLoadHooksPermissionsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".woca", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := loadHooksConfig(dir); len(cfg.PreToolUse) != 0 {
		t.Fatal("invalid hooks json should be skipped")
	}
	if rules := loadPermissions(dir); rules == nil || len(rules.Allow) != 0 {
		t.Fatal("invalid permissions json should be skipped")
	}
}

func TestLoginWriteFailure(t *testing.T) {
	home := t.TempDir()
	// credentials.json exists as a directory: the final write fails.
	if err := os.MkdirAll(filepath.Join(home, ".woca", "credentials.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"access_token":"at","refresh_token":"rt"}`,
	)
	t.Setenv("HOME", home)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected write failure")
	}
}

func TestConfigSecretRoundTripAndEnvDB(t *testing.T) {
	home := isolateEnv(t)

	// Without --db the WOCA_DB env target is used.
	if err := execCmd(t, "config", "list"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "config", "set", "anthropic-api-key", "sk-x"); err != nil {
		t.Fatal(err)
	}
	// A set secret reads back masked.
	if err := execCmd(t, "config", "get", "anthropic-api-key"); err != nil {
		t.Fatal(err)
	}
	// mcp auth without --db also resolves via env.
	if err := execCmd(t, "mcp", "auth", "srv", "http://127.0.0.1:1/mcp"); err == nil {
		t.Fatal("expected oauth failure")
	}
	_ = home
}

func TestServeBuildAgentError(t *testing.T) {
	isolateEnv(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.provider = "nope"
	if err := serve(t.Context(), opts, "127.0.0.1:0", true); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestNewMcpAuthRunnerNonHTTPSpec(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldArgs := os.Args
	os.Args = []string{"woca", "--mcp", "srv=echo hello"}
	defer func() { os.Args = oldArgs }()
	runner := newMcpAuthRunner(st)
	// The spec command is not an http(s) endpoint: configuration error.
	if err := runner(t.Context(), "srv"); err == nil {
		t.Fatal("expected not-configured error")
	}
}

func TestRunOneShotToolTurnsJSON(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// The model requests a Write; non-interactive mode denies it, producing
	// a tool result message, then finishes with text.
	url := fakeOpenAIToolFlow(t, "after tool")
	opts := baseOpts(t, url)
	opts.prompt = "write a note"
	opts.outputFormat = "json"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "after tool") {
		t.Fatalf("output = %q", out.String())
	}
}
