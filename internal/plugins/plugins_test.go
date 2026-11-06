package plugins

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writePluginFile writes content under a plugin tree, creating parent
// directories as needed.
func writePluginFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makePlugin writes a plugin tree with one skill and one agent definition.
func makePlugin(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	mkdir := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{dir}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkdir()
	manifest := `{"name":"` + name + `","version":"1.2.3","description":"test plugin"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir("skills", "deploy")
	skill := "---\nname: deploy\ndescription: Deploys things\n---\nRun the deploy.\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", "deploy", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir("agents")
	agent := "---\nname: deployer\ndescription: Deploys code\n---\nYou deploy code.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "deployer.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadPlugin(t *testing.T) {
	src := makePlugin(t, t.TempDir(), "acme")
	p, err := LoadPlugin(src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "acme" || p.Manifest.Version != "1.2.3" {
		t.Fatalf("manifest = %+v", p.Manifest)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "acme-deploy" {
		t.Fatalf("skills = %+v (want namespaced acme-deploy)", p.Skills)
	}
	if len(p.Agents) != 1 || p.Agents[0].Name != "deployer" {
		t.Fatalf("agents = %+v", p.Agents)
	}
}

func TestLoadRootSkipsNonPlugins(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "good")
	// A stray non-plugin directory and a dot-dir: both skipped silently
	// (the root also hosts built-in plugin envs without plugin.json).
	os.MkdirAll(filepath.Join(root, "stray"), 0o755)
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)
	// A dir whose manifest exists but is broken JSON is still reported.
	os.MkdirAll(filepath.Join(root, "broken"), 0o755)
	os.WriteFile(filepath.Join(root, "broken", "plugin.json"), []byte("{"), 0o644)

	list, errs := Load(root)
	if len(list) != 1 || list[0].Manifest.Name != "good" {
		t.Fatalf("list = %+v", list)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v (want only the broken manifest reported)", errs)
	}
}

func TestLoadMissingRoot(t *testing.T) {
	list, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if list != nil || errs != nil {
		t.Fatalf("missing root should be quiet: %v %v", list, errs)
	}
}

func TestInstallFromLocalPath(t *testing.T) {
	srcRoot := t.TempDir()
	src := makePlugin(t, srcRoot, "acme")
	dstRoot := filepath.Join(t.TempDir(), "plugins")

	path, err := Install(src, dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "acme" {
		t.Fatalf("installed at %s", path)
	}
	p, err := LoadPlugin(path)
	if err != nil || len(p.Skills) != 1 || len(p.Agents) != 1 {
		t.Fatalf("installed plugin = %+v err=%v", p, err)
	}
	// Reinstall replaces cleanly.
	if _, err := Install(src, dstRoot); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
}

func TestInstallRejectsMissingManifest(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "README"), []byte("not a plugin"), 0o644)
	if _, err := Install(src, filepath.Join(t.TempDir(), "plugins")); err == nil {
		t.Fatal("install without plugin.json should fail")
	}
}

func TestUninstall(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "acme")
	if err := Uninstall("acme", root); err != nil {
		t.Fatal(err)
	}
	list, _ := Load(root)
	if len(list) != 0 {
		t.Fatalf("uninstall left %d plugins", len(list))
	}
	if err := Uninstall("../escape", root); err == nil {
		t.Fatal("path separators in plugin names must be rejected")
	}
}

// The plugin root is shared with the built-in capability plugins: runtimes/
// and per-plugin environment dirs (venvs, stamps) have no plugin.json and
// must be skipped silently, not reported as broken plugins.
func TestLoadSkipsNonPluginDirs(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "deploy")
	for _, dir := range []string{
		filepath.Join(root, "runtimes", "python", "bin"),
		filepath.Join(root, "runtimes", "pinchtab", "v0.15.1"),
		filepath.Join(root, "pdf", "venv", "bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list, errs := Load(root)
	if len(errs) != 0 {
		t.Fatalf("env dirs must not produce errors: %v", errs)
	}
	if len(list) != 1 || list[0].Manifest.Name != "deploy" {
		t.Fatalf("expected only the marketplace plugin, got %+v", list)
	}
}

func TestDefaultRootAndInstallEdgeCases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := DefaultRoot(); got != filepath.Join(home, ".woca", "plugins") {
		t.Fatalf("DefaultRoot = %q", got)
	}

	root := t.TempDir()
	// A URL whose basename is empty after trimming .git.
	if _, err := Install("https://example.com/.git", root); err == nil {
		t.Fatal("expected name-derivation error")
	}
	// An unreachable git URL fails the clone and cleans up.
	if _, err := Install("http://127.0.0.1:1/repo.git", root); err == nil {
		t.Fatal("expected clone failure")
	}
	// A local path without plugin.json is rejected.
	if _, err := Install(t.TempDir(), root); err == nil {
		t.Fatal("expected missing-plugin.json error")
	}
	// A missing local path fails the stat check.
	if _, err := Install(filepath.Join(root, "missing"), root); err == nil {
		t.Fatal("expected missing-path error")
	}
	// copyTree rejects non-directories.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(blocker, root); err == nil {
		t.Fatal("expected not-a-directory error")
	}

	// A valid local plugin installs and validates.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(`{"name":"ok","version":"1.0.0","description":"d"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := Install(src, root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dst) != filepath.Base(src) {
		t.Fatalf("dst = %q", dst)
	}
	// Installing into an unwritable root fails.
	badRoot := filepath.Join(root, "file-as-root")
	if err := os.WriteFile(badRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(src, badRoot); err == nil {
		t.Fatal("expected mkdir failure")
	}
	// An invalid plugin.json fails validation and is cleaned up.
	src2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(src2, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(src2, root); err == nil {
		t.Fatal("expected validation failure")
	}
}

// TestLoadPluginContributions: a woca manifest declaring a hooks file, an
// MCP file and a commands dir — all three resolve, and ${...PLUGIN_ROOT}
// placeholders are substituted with the plugin install path.
func TestLoadPluginContributions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p1")
	writePluginFile(t, filepath.Join(dir, "plugin.json"),
		`{"name":"p1","version":"1.0.0","hooks":"hooks/hooks.json","mcp_servers":".mcp.json","commands_dir":"cmds"}`)
	writePluginFile(t, filepath.Join(dir, "hooks", "hooks.json"),
		`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"run ${CLAUDE_PLUGIN_ROOT}/check.sh"}]}]}}`)
	writePluginFile(t, filepath.Join(dir, ".mcp.json"),
		`{"mcpServers":{"ctx":{"command":"node","args":["${CLAUDE_PLUGIN_ROOT}/server.js"]}}}`)
	writePluginFile(t, filepath.Join(dir, "cmds", "deploy.md"),
		"---\ndescription: Deploy the app\n---\nDeploy $ARGUMENTS with $1 and $2.\n")

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hooks.PreToolUse) != 1 {
		t.Fatalf("hooks = %+v", p.Hooks)
	}
	entry := p.Hooks.PreToolUse[0]
	if entry.Matcher != "Bash" || len(entry.Hooks) != 1 {
		t.Fatalf("entry = %+v", entry)
	}
	if want := "run " + filepath.Join(dir, "check.sh"); entry.Hooks[0].Command != want {
		t.Fatalf("hook command = %q; want %q (PLUGIN_ROOT substituted)", entry.Hooks[0].Command, want)
	}
	if len(p.MCPSpecs) != 1 {
		t.Fatalf("mcpSpecs = %v", p.MCPSpecs)
	}
	if spec := p.MCPSpecs[0]; !strings.HasPrefix(spec, "ctx=node") || !strings.Contains(spec, filepath.Join(dir, "server.js")) {
		t.Fatalf("mcp spec = %q; want ctx=node with substituted root", spec)
	}
	if len(p.Commands) != 1 {
		t.Fatalf("commands = %+v", p.Commands)
	}
	cmd := p.Commands[0]
	if cmd.Name != "p1:deploy" {
		t.Fatalf("command name = %q; want p1:deploy", cmd.Name)
	}
	if cmd.Description != "Deploy the app" {
		t.Fatalf("command description = %q", cmd.Description)
	}
	if cmd.Body != "Deploy {arguments} with {arg1} and {arg2}." {
		t.Fatalf("command body = %q; want placeholders rewritten", cmd.Body)
	}
	var args []string
	for _, a := range cmd.Args {
		args = append(args, a.Name)
	}
	if !slices.Equal(args, []string{"arguments", "arg1", "arg2"}) {
		t.Fatalf("command args = %v; want only placeholders actually present", args)
	}
	if len(p.Ignored) != 0 {
		t.Fatalf("ignored = %v", p.Ignored)
	}
}

// A broken contribution must never yield a partial silent load: the plugin
// is skipped and its error collected (same treatment as a broken
// manifest), while intact plugins still load.
func TestLoadPluginBrokenContributionIsCollected(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "good")
	writePluginFile(t, filepath.Join(root, "badhooks", "plugin.json"), `{"name":"badhooks","hooks":42}`)

	list, errs := Load(root)
	if len(list) != 1 || list[0].Manifest.Name != "good" {
		t.Fatalf("list = %+v (badhooks must be skipped)", list)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "badhooks") {
		t.Fatalf("errs = %v; want the broken contribution surfaced", errs)
	}
}

func TestLoadPluginContributionErrors(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		files    map[string]string
	}{
		{"hooks wrong shape", `{"name":"x","hooks":42}`, nil},
		{"hooks file invalid json", `{"name":"x","hooks":"hooks/h.json"}`,
			map[string]string{"hooks/h.json": "{"}},
		{"hooks declared file missing", `{"name":"x","hooks":"hooks/nope.json"}`, nil},
		{"hooks wrapper non-object", `{"name":"x","hooks":{"hooks":"nope"}}`, nil},
		{"mcp entry missing url", `{"name":"x","mcp_servers":{"httpd":{"type":"http"}}}`, nil},
		{"mcp wrong shape", `{"name":"x","mcp_servers":17}`, nil},
		{"mcp wrapper non-object", `{"name":"x","mcp_servers":{"mcpServers":"nope"}}`, nil},
		{"commands wrong shape", `{"name":"x","commands_dir":17}`, nil},
		{"skills wrong shape", `{"name":"x","skills":{"dir":"x"}}`, nil},
		{"agents wrong shape", `{"name":"x","agents":true}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "x")
			writePluginFile(t, filepath.Join(dir, "plugin.json"), tc.manifest)
			for path, content := range tc.files {
				writePluginFile(t, filepath.Join(dir, path), content)
			}
			if _, err := LoadPlugin(dir); err == nil {
				t.Fatal("expected broken contribution to fail LoadPlugin")
			}
		})
	}
}

// TestManifestPolymorphicShapes: Claude spellings (skills, agents,
// commands) and path arrays for every contribution field.
func TestManifestPolymorphicShapes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "poly")
	writePluginFile(t, filepath.Join(dir, "plugin.json"), `{
		"name": "poly",
		"skills": "abilities",
		"agents": "bots",
		"hooks": ["h/a.json", "h/b.json"],
		"mcp_servers": ["m.json"],
		"commands": ["c1", "c2"]
	}`)
	writePluginFile(t, filepath.Join(dir, "abilities", "greeter", "SKILL.md"),
		"---\nname: greeter\ndescription: g\n---\nHi.\n")
	writePluginFile(t, filepath.Join(dir, "bots", "helper.md"),
		"---\nname: helper\ndescription: h\n---\nYou help.\n")
	writePluginFile(t, filepath.Join(dir, "h", "a.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"a"}]}]}}`)
	writePluginFile(t, filepath.Join(dir, "h", "b.json"),
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"b","timeout":1}]}]}}`)
	writePluginFile(t, filepath.Join(dir, "m.json"), `{"mcpServers":{"one":{"command":"echo"}}}`)
	writePluginFile(t, filepath.Join(dir, "c1", "alpha.md"), "Alpha $ARGUMENTS\n")
	writePluginFile(t, filepath.Join(dir, "c2", "beta.md"), "Beta $3\n")

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "poly-greeter" {
		t.Fatalf("skills = %+v (Claude 'skills' alias)", p.Skills)
	}
	if p.Manifest.SkillsDir != "abilities" {
		t.Fatalf("SkillsDir = %q", p.Manifest.SkillsDir)
	}
	if len(p.Agents) != 1 || p.Agents[0].Name != "helper" {
		t.Fatalf("agents = %+v (Claude 'agents' alias)", p.Agents)
	}
	if p.Manifest.AgentsDir != "bots" {
		t.Fatalf("AgentsDir = %q", p.Manifest.AgentsDir)
	}
	// Two hooks files merged; the Claude timeout alias normalized.
	if len(p.Hooks.PreToolUse) != 1 || len(p.Hooks.Stop) != 1 {
		t.Fatalf("hooks = %+v (list-of-paths merged)", p.Hooks)
	}
	if ms := p.Hooks.Stop[0].Hooks[0].TimeoutMS; ms != 1000 {
		t.Fatalf("TimeoutMS = %d; want alias-normalized 1000", ms)
	}
	if !slices.Equal(p.MCPSpecs, []string{"one=echo"}) {
		t.Fatalf("mcpSpecs = %v", p.MCPSpecs)
	}
	if len(p.Commands) != 2 || p.Commands[0].Name != "poly:alpha" || p.Commands[1].Name != "poly:beta" {
		t.Fatalf("commands = %+v", p.Commands)
	}
	if !strings.Contains(p.Commands[0].Body, "{arguments}") || !strings.Contains(p.Commands[1].Body, "{arg3}") {
		t.Fatalf("command bodies not rewritten: %+v", p.Commands)
	}
	var betaArgs []string
	for _, a := range p.Commands[1].Args {
		betaArgs = append(betaArgs, a.Name)
	}
	if !slices.Equal(betaArgs, []string{"arg3"}) {
		t.Fatalf("beta args = %v; want only arg3", betaArgs)
	}
}

// TestContributionDefaults: with no contribution fields declared,
// hooks/hooks.json, .mcp.json and commands/ are discovered by default.
func TestContributionDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "defaults")
	writePluginFile(t, filepath.Join(dir, "plugin.json"), `{"name":"defaults"}`)
	writePluginFile(t, filepath.Join(dir, "hooks", "hooks.json"),
		`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"tail"}]}]}}`)
	writePluginFile(t, filepath.Join(dir, ".mcp.json"),
		`{"mcpServers":{"two":{"command":"echo","args":["hi"]}}}`)
	writePluginFile(t, filepath.Join(dir, "commands", "ping.md"), "Ping $ARGUMENTS\n")

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hooks.Stop) != 1 || p.Hooks.Stop[0].Hooks[0].Command != "tail" {
		t.Fatalf("hooks = %+v (default hooks/hooks.json)", p.Hooks)
	}
	if len(p.MCPSpecs) != 1 || !strings.HasPrefix(p.MCPSpecs[0], "two=echo") {
		t.Fatalf("mcpSpecs = %v (default .mcp.json)", p.MCPSpecs)
	}
	if len(p.Commands) != 1 || p.Commands[0].Name != "defaults:ping" {
		t.Fatalf("commands = %+v (default commands/ dir)", p.Commands)
	}
}
