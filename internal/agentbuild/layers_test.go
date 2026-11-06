package agentbuild

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/merge"
	"github.com/projanvil/woca/internal/plugins"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
)

// writeFileT creates path's parent dirs and writes content.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "dup", "SKILL.md"), "USER SKILL")
	writeFileT(t, filepath.Join(root, ".claude", "skills", "dup", "SKILL.md"), "ROOT CLAUDE")
	writeFileT(t, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md"), "DEEP WOCA")
	writeFileT(t, filepath.Join(deep, ".claude", "skills", "dup", "SKILL.md"), "DEEP CLAUDE")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "only-user", "SKILL.md"), "USER ONLY")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadSkills(deep, nil, nil, zerolog.New(&buf))

	byName := map[string]skills.Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if got := byName["dup"]; len(got.Body) == 0 || got.Body != "DEEP WOCA" {
		t.Fatalf("dup winner must be workDir .woca/skills (D2 breaking flip), got %q", got.Body)
	}
	if got := byName["only-user"]; got.Body != "USER ONLY" {
		t.Fatalf("user-only skill missing: %+v", list)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", logs)
	}
	if !strings.Contains(logs, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md")) {
		t.Fatalf("winner source not in logs: %q", logs)
	}
}

func TestLoadSkillsSameDirWocaBeatsClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir() // no .git: only this directory is considered
	writeFileT(t, filepath.Join(dir, ".woca", "skills", "dup", "SKILL.md"), "WOCA")
	writeFileT(t, filepath.Join(dir, ".claude", "skills", "dup", "SKILL.md"), "CLAUDE")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	list := loadSkills(dir, []string{"none"}, nil, zerolog.Nop())
	if len(list) != 1 || list[0].Body != "WOCA" {
		t.Fatalf("same-dir .woca must beat .claude, got %+v", list)
	}
}

func TestLoadSkillsNearClaudeBeatsFarWoca(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(root, ".woca", "skills", "dup", "SKILL.md"), "FAR WOCA")
	writeFileT(t, filepath.Join(deep, ".claude", "skills", "dup", "SKILL.md"), "NEAR CLAUDE")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	list := loadSkills(deep, []string{"none"}, nil, zerolog.Nop())
	if len(list) != 1 || list[0].Body != "NEAR CLAUDE" {
		t.Fatalf("near .claude must beat far .woca, got %+v", list)
	}
}

func TestLoadAgentsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: user\n---\nUSER AGENT")
	writeFileT(t, filepath.Join(deep, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: ws\n---\nWS AGENT")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "only-user.md"), "---\nname: only-user\ndescription: u\n---\nONLY USER")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadAgents(deep, zerolog.New(&buf))

	byName := map[string]string{}
	for _, d := range list {
		byName[d.Name] = d.Prompt
	}
	if byName["dup"] != "WS AGENT" {
		t.Fatalf("dup winner must be workspace, got %q", byName["dup"])
	}
	if byName["only-user"] != "ONLY USER" {
		t.Fatalf("user-only agent missing: %+v", list)
	}
	if !strings.Contains(buf.String(), "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", buf.String())
	}
}

func TestMergeMCPSpecs(t *testing.T) {
	// mergeMCPSpecs folds in the managed layer from the real woca home —
	// pin HOME so a host that once materialized a managed overlay (e.g.
	// mcp.json with a fetch server) cannot leak into the assertion.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WOCA_HOME", "")
	var buf bytes.Buffer
	got := mergeMCPSpecs(
		[]string{"dup=flag-cmd", "flagonly=flag-cmd", "dup2=flag-a", "dup2=flag-b", "bad-no-eq"},
		[]string{"dup=ws-cmd", "wsonly=ws-cmd"},
		[]string{"dup=user-cmd", "useronly=user-cmd"},
		nil,
		zerolog.New(&buf),
	)
	slices.Sort(got)
	want := []string{"dup2=flag-a", "dup=flag-cmd", "flagonly=flag-cmd", "useronly=user-cmd", "wsonly=ws-cmd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", logs)
	}
	if !strings.Contains(logs, "invalid mcp spec") {
		t.Fatalf("invalid spec not warned: %q", logs)
	}
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name                   string
		mode                   string
		skip, accept, ro, plan bool
		want                   config.PermissionMode
	}{
		{"default", "", false, false, false, false, config.ModeDefault},
		{"bypass", "", true, false, false, false, config.ModeBypass},
		{"accept", "", false, true, false, false, config.ModeAcceptEdits},
		{"readonly", "", false, false, true, false, config.ModeReadOnly},
		{"plan", "", false, false, false, true, config.ModePlan},
		{"explicit", "bypassPermissions", false, false, false, false, config.ModeBypass},
	}
	for _, c := range cases {
		if got := resolveMode(c.mode, c.skip, c.accept, c.ro, c.plan); got != c.want {
			t.Errorf("%s: resolveMode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLoadPermissions(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".woca", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"permissions":{"deny":["Bash(rm *)"],"allow":["Bash(git *)","Read"]}}`
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := loadPermissions(dir)
	if rules == nil {
		t.Fatal("rules should not be nil")
	}
	if action, ok := rules.Action("Bash", map[string]any{"command": "rm -rf /"}); !ok || action != "deny" {
		t.Fatalf("rm should be denied: %q %v", action, ok)
	}
	if action, ok := rules.Action("Bash", map[string]any{"command": "git status"}); !ok || action != "allow" {
		t.Fatalf("git should be allowed: %q %v", action, ok)
	}
}

func TestLoadHooksConfig(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".woca", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadHooksConfig(dir, nil)
	if len(cfg.PreToolUse) != 1 || cfg.PreToolUse[0].Hooks[0].Command != "echo hi" {
		t.Fatalf("hooks = %+v", cfg)
	}
}

func TestLoadSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".woca", "skills", "commit")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: commit\ndescription: commit\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := loadSkills(dir, []string{"none"}, nil, zerolog.Nop())
	if len(list) != 1 || list[0].Name != "commit" {
		t.Fatalf("skills = %+v", list)
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

	if got := ResolveKey("flag-key", "openai", st); got != "flag-key" {
		t.Fatalf("flag wins: %q", got)
	}

	t.Setenv("OPENAI_API_KEY", "env-openai")
	if got := ResolveKey("", "openai", st); got != "env-openai" {
		t.Fatalf("openai env: %q", got)
	}
	os.Unsetenv("OPENAI_API_KEY")
	if err := st.SetSecret(settings.SecretOpenAIAPIKey, "db-openai"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveKey("", "openai", st); got != "db-openai" {
		t.Fatalf("openai db: %q", got)
	}

	t.Setenv("GLM_API_KEY", "env-glm")
	if got := ResolveKey("", "glm", st); got != "env-glm" {
		t.Fatalf("glm env: %q", got)
	}
	os.Unsetenv("GLM_API_KEY")
	if err := st.SetSecret(settings.SecretGLMAPIKey, "db-glm"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveKey("", "glm", st); got != "db-glm" {
		t.Fatalf("glm db: %q", got)
	}

	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek")
	if got := ResolveKey("", "deepseek", st); got != "env-deepseek" {
		t.Fatalf("deepseek env: %q", got)
	}
	os.Unsetenv("DEEPSEEK_API_KEY")
	if err := st.SetSecret(settings.SecretDeepSeekAPIKey, "db-deepseek"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveKey("", "deepseek", st); got != "db-deepseek" {
		t.Fatalf("deepseek db: %q", got)
	}

	// Default provider (anthropic): env, then db secret, then OAuth token.
	t.Setenv("ANTHROPIC_API_KEY", "env-anthropic")
	if got := ResolveKey("", "anthropic", st); got != "env-anthropic" {
		t.Fatalf("anthropic env: %q", got)
	}
	os.Unsetenv("ANTHROPIC_API_KEY")
	if err := st.SetSecret(settings.SecretAnthropicAPIKey, "db-anthropic"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveKey("", "anthropic", st); got != "db-anthropic" {
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
	if got := ResolveKey("", "anthropic", st); got != "oauth-tok" {
		t.Fatalf("oauth fallback: %q", got)
	}
}

func TestResolveBaseURL(t *testing.T) {
	db := settings.Settings{}
	if got := ResolveBaseURL("http://flag", "openai", db); got != "http://flag" {
		t.Fatalf("flag: %q", got)
	}

	t.Setenv("OPENAI_BASE_URL", "http://env-openai")
	if got := ResolveBaseURL("", "openai", db); got != "http://env-openai" {
		t.Fatalf("openai env: %q", got)
	}
	os.Unsetenv("OPENAI_BASE_URL")
	db.OpenAIBaseURL = "http://db-openai"
	if got := ResolveBaseURL("", "openai", db); got != "http://db-openai" {
		t.Fatalf("openai db: %q", got)
	}

	t.Setenv("GLM_BASE_URL", "http://env-glm")
	if got := ResolveBaseURL("", "glm", db); got != "http://env-glm" {
		t.Fatalf("glm env: %q", got)
	}
	os.Unsetenv("GLM_BASE_URL")
	if got := ResolveBaseURL("", "glm", settings.Settings{}); got != config.ProviderDefaultBaseURL(config.ProviderGLM) {
		t.Fatalf("glm default: %q", got)
	}

	t.Setenv("DEEPSEEK_BASE_URL", "http://env-ds")
	if got := ResolveBaseURL("", "deepseek", db); got != "http://env-ds" {
		t.Fatalf("deepseek env: %q", got)
	}
	os.Unsetenv("DEEPSEEK_BASE_URL")
	if got := ResolveBaseURL("", "deepseek", settings.Settings{}); got != config.ProviderDefaultBaseURL(config.ProviderDeepSeek) {
		t.Fatalf("deepseek default: %q", got)
	}

	t.Setenv("ANTHROPIC_BASE_URL", "http://env-an")
	if got := ResolveBaseURL("", "anthropic", db); got != "http://env-an" {
		t.Fatalf("anthropic env: %q", got)
	}
	os.Unsetenv("ANTHROPIC_BASE_URL")
	t.Setenv("ANTHROPIC_API_URL", "http://apiurl-an")
	if got := ResolveBaseURL("", "anthropic", db); got != "http://apiurl-an" {
		t.Fatalf("anthropic api url: %q", got)
	}
	os.Unsetenv("ANTHROPIC_API_URL")
	db.AnthropicBaseURL = "http://db-an"
	if got := ResolveBaseURL("", "anthropic", db); got != "http://db-an" {
		t.Fatalf("anthropic db: %q", got)
	}
}

func TestResolveBaseURLFromSettings(t *testing.T) {
	db := settings.Settings{
		GLMBaseURL:      "http://db-glm",
		DeepSeekBaseURL: "http://db-deepseek",
	}
	if got := ResolveBaseURL("", "glm", db); got != "http://db-glm" {
		t.Fatalf("glm db = %q", got)
	}
	if got := ResolveBaseURL("", "deepseek", db); got != "http://db-deepseek" {
		t.Fatalf("deepseek db = %q", got)
	}
}

func TestLoadAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".woca", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: reviewer\ndescription: reviews code\n---\nYou review code."
	if err := os.WriteFile(filepath.Join(agentsDir, "reviewer.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	list := loadAgents(dir, zerolog.Nop())
	if len(list) != 1 || list[0].Name != "reviewer" {
		t.Fatalf("agents = %+v", list)
	}
}

func TestPluginListOrEmpty(t *testing.T) {
	home := isolateEnv(t)
	if got := pluginListOrEmpty(nil); len(got) != 0 {
		t.Fatalf("empty root = %+v", got)
	}
	// A default root that is a regular file makes the load fail; the list
	// must stay empty (errors swallowed — a live skill reload must not
	// break on a broken plugin tree).
	bad := filepath.Join(home, ".woca", "plugins")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pluginListOrEmpty(nil); len(got) != 0 {
		t.Fatalf("broken root = %+v", got)
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
	if cfg := loadHooksConfig(dir, nil); len(cfg.PreToolUse) != 0 {
		t.Fatal("invalid hooks json should be skipped")
	}
	if rules := loadPermissions(dir); rules == nil || len(rules.Allow) != 0 {
		t.Fatal("invalid permissions json should be skipped")
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
	if cfg := loadHooksConfig(t.TempDir(), nil); len(cfg.PreToolUse) != 0 {
		t.Fatal("hooks should be empty without home")
	}
	if rules := loadPermissions(t.TempDir()); len(rules.Allow) != 0 {
		t.Fatal("permissions should be empty without home")
	}
	if got := loadSkills(t.TempDir(), []string{"none"}, nil, zerolog.Nop()); len(got) != 0 {
		t.Fatal("skills should be empty without home")
	}
	if got := loadAgents(t.TempDir(), zerolog.Nop()); len(got) != 0 {
		t.Fatal("agents should be empty without home")
	}
}

func TestLoadSkillsBuiltinLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	// Default (empty allowlist): the vendored set is present.
	list := loadSkills(dir, nil, nil, zerolog.Nop())
	var tdd skills.Skill
	found := false
	for _, s := range list {
		if s.Name == "test-driven-development" {
			tdd, found = s, true
		}
	}
	if !found || !strings.Contains(tdd.Body, "RED") {
		t.Fatalf("builtin test-driven-development missing or empty: found=%v", found)
	}

	// A non-empty allowlist keeps only the named skills.
	only := loadSkills(dir, []string{"brainstorming"}, nil, zerolog.Nop())
	if len(only) != 1 || only[0].Name != "brainstorming" {
		t.Fatalf("allowlist not applied: %+v", only)
	}

	// A user skill of the same name shadows the builtin, and the conflict
	// log records the builtin as the loser.
	writeFileT(t, filepath.Join(home, ".woca", "skills", "test-driven-development", "SKILL.md"), "USER TDD")
	var buf bytes.Buffer
	list = loadSkills(dir, nil, nil, zerolog.New(&buf))
	for _, s := range list {
		if s.Name == "test-driven-development" && s.Body != "USER TDD" {
			t.Fatalf("user skill must shadow the builtin, got %q", s.Body)
		}
	}
	if !strings.Contains(buf.String(), "builtin/test-driven-development") {
		t.Fatalf("builtin loser not logged: %q", buf.String())
	}
}

// pluginFixture writes a plugin tree carrying every contribution kind —
// two skills, a command, a hooks entry and an MCP server named after the
// plugin — under root/name, and loads it ready to inject into the layered
// loaders (tests inject the plugin list directly instead of reaching for
// the ~/.woca/plugins default root).
func pluginFixture(t *testing.T, root, name string) plugins.Plugin {
	t.Helper()
	dir := filepath.Join(root, name)
	writeFileT(t, filepath.Join(dir, "plugin.json"), `{"name":"`+name+`"}`)
	writeFileT(t, filepath.Join(dir, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: Deploys things\n---\nPLUGIN DEPLOY")
	writeFileT(t, filepath.Join(dir, "skills", "lint", "SKILL.md"),
		"---\nname: lint\ndescription: Lints things\n---\nPLUGIN LINT")
	writeFileT(t, filepath.Join(dir, "commands", "ping.md"), "Ping $ARGUMENTS\n")
	writeFileT(t, filepath.Join(dir, "hooks", "hooks.json"),
		`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo plugin-hook"}]}]}}`)
	writeFileT(t, filepath.Join(dir, ".mcp.json"),
		`{"mcpServers":{"`+name+`":{"command":"echo","args":["hi"]}}}`)
	pl, err := plugins.LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// TestLoadSkillsPluginLayer: plugin skills enter the merge at the plugin
// level (between managed and user). A user skill of the same namespaced
// name wins and the conflict log names the plugin layer as the loser;
// plugin-only skills survive, and plugin commands append after the merge
// under their "<plugin>:<command>" namespace (no conflicts by design).
func TestLoadSkillsPluginLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)
	pl := pluginFixture(t, t.TempDir(), "toolkit")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "toolkit-deploy", "SKILL.md"),
		"---\nname: toolkit-deploy\ndescription: user copy\n---\nUSER DEPLOY")

	var buf bytes.Buffer
	list := loadSkills(workdir, []string{"none"}, []plugins.Plugin{pl}, zerolog.New(&buf))

	byName := map[string]skills.Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if got := byName["toolkit-deploy"]; got.Body != "USER DEPLOY" {
		t.Fatalf("user skill must shadow the plugin skill, got %q", got.Body)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflict not logged: %q", logs)
	}
	if !strings.Contains(logs, "plugin/toolkit") {
		t.Fatalf("plugin layer not named in the conflict log: %q", logs)
	}
	if got := byName["toolkit-lint"]; got.Body != "PLUGIN LINT" {
		t.Fatalf("plugin-only skill missing: %+v", list)
	}
	if got := byName["toolkit:ping"]; len(got.Body) == 0 {
		t.Fatalf("plugin command not appended after merge: %+v", list)
	}
}

// TestLoadHooksConfigPluginEntries: plugin hook entries append after the
// project's entries for the same event (they run together, no dedupe) and
// every plugin entry carries Source "plugin/<name>" for the /hooks display.
func TestLoadHooksConfigPluginEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	writeFileT(t, filepath.Join(workdir, ".woca", "settings.json"),
		`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo project"}]}]}}`)
	pl := pluginFixture(t, t.TempDir(), "toolkit")

	cfg := loadHooksConfig(workdir, []plugins.Plugin{pl})
	if len(cfg.PreToolUse) != 2 {
		t.Fatalf("PreToolUse = %+v; want project entry plus plugin entry", cfg.PreToolUse)
	}
	if cfg.PreToolUse[0].Hooks[0].Command != "echo project" {
		t.Fatalf("project entry must run first: %+v", cfg.PreToolUse)
	}
	plug := cfg.PreToolUse[1]
	if plug.Hooks[0].Command != "echo plugin-hook" {
		t.Fatalf("plugin entry must run after the project entries: %+v", cfg.PreToolUse)
	}
	if plug.Source != "plugin/toolkit" {
		t.Fatalf("plugin entry Source = %q; want plugin/toolkit", plug.Source)
	}
	if cfg.PreToolUse[0].Source != "" {
		t.Fatalf("project entry must stay untagged: %+v", cfg.PreToolUse[0])
	}
}

// TestMergeMCPSpecsPluginLayer: the plugin MCP layer sits below the user
// layer — a same-name user spec survives the merge, plugin-only names fill
// gaps, and the conflict is logged.
func TestMergeMCPSpecsPluginLayer(t *testing.T) {
	var buf bytes.Buffer
	got := mergeMCPSpecsLayered(
		nil,
		nil,
		[]string{"ctx=user-cmd", "useronly=user-cmd"},
		[]string{"ctx=plugin-cmd", "plugonly=plugin-cmd"},
		nil,
		zerolog.New(&buf),
	)
	slices.Sort(got)
	want := []string{"ctx=user-cmd", "plugonly=plugin-cmd", "useronly=user-cmd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", buf.String())
	}
}

// TestDisabledPluginContributesNothing: LoadWithSkipped drops the disabled
// plugin from the list the loaders consume, so it contributes no skills
// (nor commands), no hooks and no MCP specs.
func TestDisabledPluginContributesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	pluginFixture(t, root, "toolkit")
	pluginFixture(t, root, "other")
	workdir := t.TempDir()
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	list, skipped, errs := plugins.LoadWithSkipped(root, []string{"toolkit"})
	if len(errs) != 0 || !slices.Equal(skipped, []string{"toolkit"}) ||
		len(list) != 1 || list[0].Manifest.Name != "other" {
		t.Fatalf("LoadWithSkipped = list:%+v skipped:%v errs:%v", list, skipped, errs)
	}

	for _, s := range loadSkills(workdir, []string{"none"}, list, zerolog.Nop()) {
		if strings.HasPrefix(s.Name, "toolkit") {
			t.Fatalf("disabled plugin skill or command leaked: %+v", s)
		}
	}
	for _, e := range loadHooksConfig(workdir, list).PreToolUse {
		if e.Source == "plugin/toolkit" {
			t.Fatalf("disabled plugin hook leaked: %+v", e)
		}
	}
	var specs []string
	for _, pl := range list {
		specs = append(specs, pl.MCPSpecs...)
	}
	if got := mergeMCPSpecsLayered(nil, nil, nil, specs, nil, zerolog.Nop()); !slices.Equal(got, []string{"other=echo hi"}) {
		t.Fatalf("disabled plugin mcp spec leaked: %v", got)
	}
}
