// Package plugins implements woca's plugin loader: a plugin is a directory
// containing a plugin.json manifest (or a Claude-style
// .claude-plugin/plugin.json) plus optional skills/, agents/ and commands/
// subdirectories, hook configuration and MCP servers.
// Plugins are installed under ~/.woca/plugins (from a local path or a git
// URL — the marketplace-level distribution unit) and their contributions
// merge into the session at startup.
package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/skills"
)

// Manifest is the plugin.json contract. Contribution fields accept woca
// and Claude spellings (skills_dir|skills, agents_dir|agents,
// commands_dir|commands) and may be a path or a list of paths; the
// aliases are resolved in manifest.go, which records the first declared
// dir here for display.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	// SkillsDir overrides the skills subdirectory (default "skills").
	SkillsDir string `json:"skills_dir,omitempty"`
	// AgentsDir overrides the agents subdirectory (default "agents").
	AgentsDir string `json:"agents_dir,omitempty"`
}

// Plugin is a loaded plugin with its contributions resolved.
type Plugin struct {
	Manifest Manifest
	Path     string
	Skills   []skills.Skill
	Agents   []agents.Definition
	// Commands are the plugin's commands (commands/*.md) exposed as
	// skills named "<plugin>:<command>".
	Commands []skills.Skill
	// Hooks is the plugin's hooks configuration: entries are tagged with
	// their Source and ${...PLUGIN_ROOT} placeholders are already
	// substituted with Path.
	Hooks hooks.Config
	// MCPSpecs are woca --mcp specs ("name=command" / "name=url") for
	// the plugin's MCP servers, substitution applied.
	MCPSpecs []string
	// Ignored lists unsupported Claude/Codex contribution keys found in
	// the manifest (surfaced as warnings by /plugins).
	Ignored []string
}

// DefaultRoot returns the plugin installation root (~/.woca/plugins).
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "plugins")
}

// LoadPlugin loads one plugin directory: manifest plus contributions
// (skills, agents, commands, hooks, MCP servers). The manifest is
// plugin.json or .claude-plugin/plugin.json (first wins; the name
// defaults to the directory base). Missing contribution directories are
// fine; a missing manifest or a declared-but-broken contribution is an
// error — plugins never load partially and silently.
func LoadPlugin(dir string) (Plugin, error) {
	m, err := readManifest(dir)
	if err != nil {
		return Plugin{}, err
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	p := Plugin{
		Manifest: Manifest{Name: m.Name, Version: m.Version, Description: m.Description},
		Path:     dir,
	}
	if err := resolveContributions(&p, m, dir); err != nil {
		return Plugin{}, fmt.Errorf("plugins: %s: %w", dir, err)
	}
	return p, nil
}

// hasManifest reports whether dir carries a plugin manifest in either
// supported location.
func hasManifest(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	return err == nil
}

// Load walks a plugin root and loads every plugin directory. A directory
// without plugin.json is not a plugin and is skipped silently — the root
// also hosts non-plugin state such as the built-in capability plugins'
// runtimes/ dir and their per-plugin environments. A plugin with a broken
// manifest is skipped (its error is collected for diagnostics).
func Load(root string) ([]Plugin, []error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []Plugin
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "runtimes" {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !hasManifest(dir) {
			continue // no manifest: built-in plugin environment or unrelated state
		}
		p, err := LoadPlugin(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	return out, errs
}

// Install installs a plugin from a local directory or a git URL into root.
// Local paths are copied (excluding .git); URLs are cloned with --depth 1.
// The returned path is the installed plugin directory.
func Install(src, root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	name := ""
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "git@") {
		base := strings.TrimSuffix(filepath.Base(src), ".git")
		if base == "" {
			return "", fmt.Errorf("plugins: cannot derive name from %s", src)
		}
		name = base
		dst := filepath.Join(root, name)
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
		cmd := exec.Command("git", "clone", "--depth", "1", src, dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dst)
			return "", fmt.Errorf("plugins: git clone: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := os.RemoveAll(filepath.Join(dst, ".git")); err != nil {
			return "", err
		}
		return validateInstalled(dst)
	}

	// Local path: copy the tree.
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	if !hasManifest(abs) {
		return "", fmt.Errorf("plugins: %s has no plugin.json", src)
	}
	name = filepath.Base(abs)
	dst := filepath.Join(root, name)
	if err := copyTree(abs, dst); err != nil {
		return "", err
	}
	return validateInstalled(dst)
}

func validateInstalled(dst string) (string, error) {
	if !hasManifest(dst) {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("plugins: installed plugin has no plugin.json: %w", os.ErrNotExist)
	}
	if _, err := LoadPlugin(dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// Uninstall removes a plugin by name from root.
func Uninstall(name, root string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("plugins: invalid plugin name %q", name)
	}
	return os.RemoveAll(filepath.Join(root, name))
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("plugins: %s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
