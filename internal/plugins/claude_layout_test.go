package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/hooks"
)

// Tests for the Claude Code plugin layout: .claude-plugin/plugin.json
// manifests, inline hooks/mcpServers objects, the commands/ dir and
// ignored (unsupported) contribution keys.

// TestClaudeLayoutInlineContributions: .claude-plugin/plugin.json with
// inline hooks and mcpServers objects, a commands dir and an unsupported
// lspServers key — the same contributions as the woca layout, plus the
// ignored key surfaced.
func TestClaudeLayoutInlineContributions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "claudep")
	writePluginFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{
		"name": "claudep",
		"version": "2.0.0",
		"commands": "commands",
		"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "run ${WOCA_PLUGIN_ROOT}/guard.sh"}]}]},
		"mcpServers": {"db": {"command": "postgres-mcp", "args": ["--root", "${CLAUDE_PLUGIN_ROOT}"]}},
		"lspServers": {"go": {"command": "gopls"}}
	}`)
	writePluginFile(t, filepath.Join(dir, "commands", "deploy.md"),
		"---\ndescription: Deploy\n---\nDeploy $ARGUMENTS to $1.\n")

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "claudep" || p.Manifest.Version != "2.0.0" {
		t.Fatalf("manifest = %+v", p.Manifest)
	}
	if cmd := p.Hooks.PreToolUse[0].Hooks[0].Command; cmd != "run "+filepath.Join(dir, "guard.sh") {
		t.Fatalf("hook command = %q; want WOCA_PLUGIN_ROOT substituted with %q", cmd, dir)
	}
	if src := p.Hooks.PreToolUse[0].Source; src != "claudep" {
		t.Fatalf("entry Source = %q; want plugin name", src)
	}
	if len(p.MCPSpecs) != 1 {
		t.Fatalf("mcpSpecs = %v", p.MCPSpecs)
	}
	if spec := p.MCPSpecs[0]; !strings.Contains(spec, "db=postgres-mcp --root") || !strings.Contains(spec, dir) {
		t.Fatalf("mcp spec = %q; want db server with substituted root", spec)
	}
	if len(p.Commands) != 1 || p.Commands[0].Name != "claudep:deploy" || !strings.Contains(p.Commands[0].Body, "{arguments}") {
		t.Fatalf("commands = %+v", p.Commands)
	}
	if !slices.Equal(p.Ignored, []string{"lspServers"}) {
		t.Fatalf("Ignored = %v; want [lspServers]", p.Ignored)
	}

	// The root-level walk recognizes the .claude-plugin layout as a plugin.
	list, errs := Load(root)
	if len(errs) != 0 || len(list) != 1 || list[0].Manifest.Name != "claudep" {
		t.Fatalf("Load = %+v %v", list, errs)
	}
}

// TestClaudeLayoutWrapperAndSourceForms: inline objects may also use the
// {"hooks": {...}} / {"mcpServers": {...}} wrapper shape or carry a
// {"source": "<path>"} reference to an external file.
func TestClaudeLayoutWrapperAndSourceForms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wrappers")
	writePluginFile(t, filepath.Join(dir, "plugin.json"), `{
		"name": "wrappers",
		"hooks": {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "stop"}]}]}},
		"mcpServers": {"mcpServers": {"one": {"command": "echo"}}}
	}`)
	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hooks.Stop) != 1 || p.Hooks.Stop[0].Hooks[0].Command != "stop" {
		t.Fatalf("hooks = %+v (wrapper form)", p.Hooks)
	}
	if !slices.Equal(p.MCPSpecs, []string{"one=echo"}) {
		t.Fatalf("mcpSpecs = %v (wrapper form)", p.MCPSpecs)
	}

	ref := filepath.Join(t.TempDir(), "refd")
	writePluginFile(t, filepath.Join(ref, "hooks", "ext.json"),
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"boot"}]}]}}`)
	writePluginFile(t, filepath.Join(ref, "plugin.json"), `{
		"name": "refd",
		"hooks": {"source": "hooks/ext.json"}
	}`)
	p2, err := LoadPlugin(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Hooks.SessionStart) != 1 || p2.Hooks.SessionStart[0].Hooks[0].Command != "boot" {
		t.Fatalf("hooks = %+v (source reference form)", p2.Hooks)
	}
}

// TestClaudeLayoutNameDefaultsToDirBase: an unnamed .claude-plugin
// manifest inherits the directory base name (used for command names too).
func TestClaudeLayoutNameDefaultsToDirBase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "basename")
	writePluginFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"version":"1.0.0"}`)
	writePluginFile(t, filepath.Join(dir, "commands", "go.md"), "Go $ARGUMENTS\n")

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "basename" {
		t.Fatalf("name = %q; want directory base", p.Manifest.Name)
	}
	if len(p.Commands) != 1 || p.Commands[0].Name != "basename:go" {
		t.Fatalf("commands = %+v", p.Commands)
	}
}

// TestIgnoredKeysCollected: every unsupported Claude/Codex key present in
// a manifest is listed in Ignored, in canonical order.
func TestIgnoredKeysCollected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "noisy")
	writePluginFile(t, filepath.Join(dir, "plugin.json"), `{
		"name":"noisy",
		"lspServers": {}, "outputStyles": {}, "workflows": {}, "types": {},
		"experimental": {}, "apps": {}, "appTemplates": {}, "scheduledTasks": {},
		"user_config": {}
	}`)
	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lspServers", "outputStyles", "workflows", "types",
		"experimental", "apps", "appTemplates", "scheduledTasks", "user_config"}
	if !slices.Equal(p.Ignored, want) {
		t.Fatalf("Ignored = %v; want %v", p.Ignored, want)
	}
}

// TestManifestLocationPrecedence: plugin.json in the plugin root wins
// over .claude-plugin/plugin.json when both exist.
func TestManifestLocationPrecedence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "both")
	writePluginFile(t, filepath.Join(dir, "plugin.json"), `{"name":"both","version":"1.0.0"}`)
	writePluginFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"both","version":"9.9.9"}`)

	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Version != "1.0.0" {
		t.Fatalf("version = %q; plugin.json must win", p.Manifest.Version)
	}
}

// TestBareManifestNoContributions: a manifest without any of the new
// fields behaves exactly as before — all contribution fields stay
// zero values.
func TestBareManifestNoContributions(t *testing.T) {
	src := makePlugin(t, t.TempDir(), "acme")
	p, err := LoadPlugin(src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Commands != nil || p.MCPSpecs != nil || p.Ignored != nil {
		t.Fatalf("contributions should be zero values: %+v", p)
	}
	if !reflect.DeepEqual(p.Hooks, hooks.Config{}) {
		t.Fatalf("hooks should be zero value: %+v", p.Hooks)
	}
}

// TestInstallClaudeLayout: a source tree with only
// .claude-plugin/plugin.json installs and validates like a woca plugin.
func TestInstallClaudeLayout(t *testing.T) {
	src := filepath.Join(t.TempDir(), "claudesrc")
	writePluginFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"claudesrc"}`)
	writePluginFile(t, filepath.Join(src, "commands", "x.md"), "X $ARGUMENTS\n")

	root := filepath.Join(t.TempDir(), "plugins")
	path, err := Install(src, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("installed layout: %v", err)
	}
	p, err := LoadPlugin(path)
	if err != nil || len(p.Commands) != 1 || p.Commands[0].Name != "claudesrc:x" {
		t.Fatalf("installed claude-layout plugin = %+v err=%v", p, err)
	}
}
