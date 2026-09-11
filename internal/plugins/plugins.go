// Package plugins implements woca's plugin loader: a plugin is a directory
// containing a plugin.json manifest plus optional skills/ and agents/
// subdirectories.
// Plugins are installed under ~/.woca/plugins (from a local path or a git
// URL — the marketplace-level distribution unit) and their skills and
// subagent definitions merge into the session at startup.
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/skills"
)

// Manifest is the plugin.json contract.
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
}

// DefaultRoot returns the plugin installation root (~/.woca/plugins).
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "plugins")
}

// LoadPlugin loads one plugin directory: manifest plus skill/agent
// contributions. Missing subdirectories are fine; a missing manifest is an
// error (the directory is not a plugin).
func LoadPlugin(dir string) (Plugin, error) {
	data, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return Plugin{}, fmt.Errorf("plugins: %s: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Plugin{}, fmt.Errorf("plugins: parse %s/plugin.json: %w", dir, err)
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	p := Plugin{Manifest: m, Path: dir}

	skillsDir := m.SkillsDir
	if skillsDir == "" {
		skillsDir = "skills"
	}
	if st, err := os.Stat(filepath.Join(dir, skillsDir)); err == nil && st.IsDir() {
		if list, err := skills.Load(filepath.Join(dir, skillsDir)); err == nil {
			for i := range list {
				list[i].Name = m.Name + "-" + list[i].Name // namespace plugin skills
			}
			p.Skills = list
		}
	}

	agentsDir := m.AgentsDir
	if agentsDir == "" {
		agentsDir = "agents"
	}
	if st, err := os.Stat(filepath.Join(dir, agentsDir)); err == nil && st.IsDir() {
		if list, err := agents.Load(filepath.Join(dir, agentsDir)); err == nil {
			p.Agents = list
		}
	}
	return p, nil
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
		if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err != nil {
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
	if _, err := os.Stat(filepath.Join(abs, "plugin.json")); err != nil {
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
	if _, err := os.ReadFile(filepath.Join(dst, "plugin.json")); err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("plugins: installed plugin has no plugin.json: %w", err)
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
