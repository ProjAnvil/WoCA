package main

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// http_Mux starts a test server mapping paths to canned JSON bodies.
func http_Mux(t *testing.T, routes ...string) string {
	t.Helper()
	mux := http.NewServeMux()
	for i := 0; i+1 < len(routes); i += 2 {
		body := routes[i+1]
		mux.HandleFunc(routes[i], func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(body, "invalid_grant") {
				w.WriteHeader(http.StatusBadRequest)
			}
			w.Write([]byte(body))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRunREPLCommands drives the console command surface through the REPL so
// the closures wired in newConsole (model factory, quota, thinking, theme,
// settings rebuilds, memory workspace) execute.
func TestRunREPLCommands(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "cmd-ok")
	opts := baseOpts(t, url)

	input := strings.Join([]string{
		"/model other-model",
		"/thinking 2048",
		"/quota",
		"/theme dark",
		"/theme no-such-theme",
		"/vim",
		"/vim",
		"/settings",
		"/settings help",
		"/settings get provider",
		"/settings set provider openai",
		"/settings set memory-engine none",
		"/settings set no-such-key x",
		"/memory",
		"/undo",
		"/redo",
		"/cost",
		"/context",
		"/permissions",
		"/permissions bypassPermissions",
		"/status",
		"/clear",
		"/agents",
		"run the model please",
		"/exit",
	}, "\n") + "\n"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(input), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	combined := out.String() + errBuf.String()
	for _, want := range []string{"cmd-ok", "woca", "provider"} {
		if !strings.Contains(combined, want) {
			t.Errorf("missing %q in REPL output", want)
		}
	}
}

func TestRunREPLDebugLog(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "dbg")
	opts := baseOpts(t, url)
	opts.debugLog = true

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	// The observability log directory exists under the isolated home.
	if _, err := os.Stat(filepath.Join(home, ".woca", "logs")); err != nil {
		t.Fatalf("obs log dir: %v", err)
	}
}

func TestBuildAgentDefaultsWorkdir(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	dir := t.TempDir()
	t.Chdir(dir)
	opts := baseOpts(t, url)
	opts.workdir = "" // falls back to the process working directory
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if !strings.HasPrefix(ap.cfg.WorkDir, dir) {
		t.Fatalf("workdir fallback = %q", ap.cfg.WorkDir)
	}
}

func TestBuildAgentSettingsOpenFailure(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The database path sits under a regular file: settings.Open fails and
	// buildAgent continues without persisted settings.
	t.Setenv("WOCA_DB", filepath.Join(blocker, "woca.db"))
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.settings != nil {
		t.Fatal("settings should degrade to nil")
	}
}

func TestBuildAgentManagedUnreadableAndSkills(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-skills", `["myutil"]`},
	)
	url := fakeOpenAI(t, "hi")

	// Unreadable managed settings are warned about and ignored.
	opts := baseOpts(t, url)
	opts.managedSettings = filepath.Join(home, "missing-managed.json")
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ap.pluginMgr.Close()
	ap.a.Close()

	// A ~/.claude skill selected in settings is symlinked into ~/.woca/skills.
	claudeSkill := filepath.Join(home, ".claude", "skills", "myutil")
	if err := os.MkdirAll(claudeSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkill, "SKILL.md"), []byte("---\nname: myutil\ndescription: helper\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts2 := baseOpts(t, url)
	ap2, err := buildAgent(t.Context(), opts2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap2.pluginMgr.Close()
	defer ap2.a.Close()
	if _, err := os.Lstat(filepath.Join(home, ".woca", "skills", "myutil")); err != nil {
		t.Fatalf("imported skill missing: %v", err)
	}
}

func TestBuildAgentBrokenPluginAndClaudeMCP(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-mcp", `["alpha"]`},
	)
	url := fakeOpenAI(t, "hi")

	// A plugin with an invalid manifest is reported and skipped.
	pluginRoot := filepath.Join(home, "plugins", "broken")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A selected claude MCP server gets materialized and connected (its
	// stdio command fails the MCP handshake, which is warned and skipped).
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{
		"mcpServers": {"alpha": {"command": "echo", "args": ["hi"]}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestBuildAgentPostgresBackends(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Load()
	for _, kv := range [][2]string{
		{"memory-engine", "none"},
		{"checkpoint", "postgres"},
		{"memory", "postgres"},
	} {
		if err := settings.Apply(&cfg, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// A postgres DSN pointing at a closed local port: opens fail fast and
	// both backends degrade to nil.
	if err := st.SetSecret(settings.SecretPostgresURL, "postgres://u:p@127.0.0.1:1/db"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.memory != nil {
		t.Fatal("postgres memory backend should degrade to nil")
	}
}

func TestBuildAgentMemoryEngineBuildFailure(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default engine (ladym) with an unusable DB path: the build fails and
	// buildAgent continues without the memory engine.
	seedSettings(t, [2]string{"ladym-db-path", filepath.Join(blocker, "ladym.db")})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	if ap.memEngine != nil {
		t.Fatal("broken engine path should degrade to nil")
	}
}

func TestBuildMemoryEngineBadPath(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyLadyMDBPath, filepath.Join(blocker, "ladym.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildMemoryEngine(&app{cfg: config.Config{WorkDir: t.TempDir()}, settings: st}, cfg, nil); err == nil {
		t.Fatal("expected engine build error")
	}
}

func TestLoginFailures(t *testing.T) {
	isolateEnv(t)

	// Device endpoint down: BeginDeviceFlow fails.
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", "http://127.0.0.1:1/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", "http://127.0.0.1:1/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected device-flow error")
	}
}

func TestLoginTokenRejected(t *testing.T) {
	isolateEnv(t)
	// The device endpoint works; the token endpoint rejects with a
	// non-pending error, failing the poll quickly.
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"error":"invalid_grant"}`,
	)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected token error")
	}
}

func TestMcpAuthFlowCancelled(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")
	// The authorization server discovery works, but no browser visit ever
	// happens (WOCA_NO_BROWSER=1), so the callback wait hits the context
	// deadline.
	mux := http_Mux(t,
		"/.well-known/oauth-authorization-server", `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`,
		"/register", `{"client_id":"dyn-1"}`,
	)
	root := newRootCmd()
	root.SetArgs([]string{"--db", db, "mcp", "auth", "srv", mux + "/mcp"})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err == nil {
		t.Fatal("expected cancelled flow error")
	}
}

func TestSyncMCPConfigBadClaudeJSON(t *testing.T) {
	home := isolateEnv(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncMCPConfig(filepath.Join(home, ".woca"), []string{"x"}); err == nil {
		t.Fatal("expected claude.json parse error")
	}
}

func TestApplyClaudeEnvSyncFailure(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	// Replace the skills directory with a regular file: the skill sync
	// fails on the next apply.
	if err := os.RemoveAll(filepath.Join(home, ".woca", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ap.applyClaudeEnv([]string{"s"}, nil); err == nil {
		t.Fatal("expected sync failure")
	}
}

func TestRebuildModelInvalidProvider(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	ap, err := buildAgent(t.Context(), options{workdir: t.TempDir(), apiKey: "k", baseURL: url}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
	cfg := ap.settings.Load()
	// The settings layer rejects an unknown provider before it can ever
	// reach rebuildModel.
	if err := settings.Apply(&cfg, settings.KeyProvider, "nope"); err == nil {
		t.Fatal("expected provider validation error at Apply")
	}
}

func TestHomeUnsetBranches(t *testing.T) {
	t.Setenv("HOME", "")
	if got := wocaRoot(); got != ".woca" {
		t.Fatalf("wocaRoot = %q", got)
	}
	if got := loadOAuthAccessToken(); got != "" {
		t.Fatalf("loadOAuthAccessToken = %q", got)
	}
	if cfg := loadHooksConfig(t.TempDir()); len(cfg.PreToolUse) != 0 {
		t.Fatal("hooks should be empty without home")
	}
	if rules := loadPermissions(t.TempDir()); len(rules.Allow) != 0 {
		t.Fatal("permissions should be empty without home")
	}
	if got := loadSkills(t.TempDir(), zerolog.Nop()); len(got) != 0 {
		t.Fatal("skills should be empty without home")
	}
	if got := loadAgents(t.TempDir()); len(got) != 0 {
		t.Fatal("agents should be empty without home")
	}
}

func TestIsTerminalStdin(t *testing.T) {
	// os.Stdin implements Fd(); under go test it is not a TTY.
	if isTerminal(os.Stdin) {
		t.Fatal("test stdin should not be a terminal")
	}
}

func TestInterruptHandlerAskUserShortInput(t *testing.T) {
	var stderr bytes.Buffer
	// Two questions but input for only one: the second read errors and the
	// answer is appended empty.
	h := interruptHandler(bufio.NewReader(strings.NewReader("one\n")), true, &stderr)
	ans, err := h(agent.AskUserRequest{Questions: []tools.Question{{Question: "a"}, {Question: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	aa := ans.(agent.AskUserAnswer)
	if len(aa.Answers) != 2 || aa.Answers[0] != "one" || aa.Answers[1] != "" {
		t.Fatalf("answers = %#v", aa.Answers)
	}
}

func TestConnectMCPWithToken(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSecret("mcp_secure_token", "tok"); err != nil {
		t.Fatal(err)
	}
	// The stored token path builds an auth-capable HTTP client; the
	// unreachable endpoint fails initialization.
	got, clients := connectMCP(t.Context(), testLogger(), []string{"secure=http://127.0.0.1:1/mcp"}, st)
	if len(got) != 0 || len(clients) != 0 {
		t.Fatalf("token connect = %d tools", len(got))
	}
}

func TestSaveLastSessionNoHome(t *testing.T) {
	// HOME unset: DefaultDir is "" and the snapshot lands relative to the
	// (temp) working directory without crashing.
	t.Setenv("HOME", "")
	t.Chdir(t.TempDir())
	saveLastSession([]messages.Message{messages.Human("x")}, nil)
}
