package agentbuild

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/merge"
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
	list := loadSkills(deep, nil, zerolog.New(&buf))

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

	list := loadSkills(dir, []string{"none"}, zerolog.Nop())
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

	list := loadSkills(deep, []string{"none"}, zerolog.Nop())
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
		zerolog.New(&buf),
	)
	sort.Strings(got)
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
	cfg := loadHooksConfig(dir)
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
	list := loadSkills(dir, []string{"none"}, zerolog.Nop())
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
	if got := loadSkills(t.TempDir(), []string{"none"}, zerolog.Nop()); len(got) != 0 {
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
	list := loadSkills(dir, nil, zerolog.Nop())
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
	only := loadSkills(dir, []string{"brainstorming"}, zerolog.Nop())
	if len(only) != 1 || only[0].Name != "brainstorming" {
		t.Fatalf("allowlist not applied: %+v", only)
	}

	// A user skill of the same name shadows the builtin, and the conflict
	// log records the builtin as the loser.
	writeFileT(t, filepath.Join(home, ".woca", "skills", "test-driven-development", "SKILL.md"), "USER TDD")
	var buf bytes.Buffer
	list = loadSkills(dir, nil, zerolog.New(&buf))
	for _, s := range list {
		if s.Name == "test-driven-development" && s.Body != "USER TDD" {
			t.Fatalf("user skill must shadow the builtin, got %q", s.Body)
		}
	}
	if !strings.Contains(buf.String(), "builtin/test-driven-development") {
		t.Fatalf("builtin loser not logged: %q", buf.String())
	}
}
