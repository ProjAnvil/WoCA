package console

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/plugins"
)

// pluginsCmd implements /plugins: list installed plugins with their
// contribution counts, install from a local path, git URL or marketplace,
// remove one, or enable/disable one through the disabled-plugins settings
// key. Installed agents merge into the live session; skills, hooks and MCP
// servers load on restart.
func (c *Console) pluginsCmd(args []string) (Result, error) {
	root := c.deps.PluginsRoot
	if root == "" {
		root = plugins.DefaultRoot()
	}
	if len(args) == 0 || args[0] == "list" {
		return c.pluginsList(root)
	}

	switch args[0] {
	case "install":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /plugins install <path-or-git-url-or-name@marketplace>")
		}
		path, err := plugins.Install(args[1], root)
		if err != nil {
			return Result{}, err
		}
		p, err := plugins.LoadPlugin(path)
		if err != nil {
			return Result{}, err
		}
		// Agents contributed by the plugin are usable immediately.
		if len(p.Agents) > 0 {
			c.deps.Agent.SetAgentDefinitions(append(c.deps.Agent.AgentDefinitions(), p.Agents...))
		}
		out := fmt.Sprintf("installed %s", p.Manifest.Name)
		var restart []string
		if len(p.Skills) > 0 {
			restart = append(restart, fmt.Sprintf("%d skills", len(p.Skills)))
		}
		if len(p.Commands) > 0 {
			restart = append(restart, fmt.Sprintf("%d commands", len(p.Commands)))
		}
		if n := hookEntries(p.Hooks); n > 0 {
			restart = append(restart, fmt.Sprintf("%d hooks", n))
		}
		if len(p.MCPSpecs) > 0 {
			restart = append(restart, fmt.Sprintf("%d MCP servers", len(p.MCPSpecs)))
		}
		if len(restart) > 0 {
			out += fmt.Sprintf(" (%s load on restart)", strings.Join(restart, ", "))
		}
		return Result{Output: out + "\n"}, nil

	case "rm", "remove":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /plugins rm <name>")
		}
		if err := plugins.Uninstall(args[1], root); err != nil {
			return Result{}, err
		}
		return Result{Output: "removed " + args[1] + "\n"}, nil

	case "enable", "disable":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /plugins %s <name>", args[0])
		}
		return c.pluginsToggle(root, args[0] == "disable", args[1])

	default:
		return Result{}, fmt.Errorf("unknown /plugins subcommand %q (list | install | rm | enable | disable)", args[0])
	}
}

// pluginsList renders the installed plugins: enabled ones with their
// contribution counts and ⚠ lines for ignored manifest keys, disabled ones
// (skipped at load) with a marker so they can be re-enabled by name.
func (c *Console) pluginsList(root string) (Result, error) {
	list, skipped, errs := plugins.LoadWithSkipped(root, c.disabledPlugins())
	var b strings.Builder
	for _, err := range errs {
		fmt.Fprintf(&b, "  ⚠ %v\n", err)
	}
	if len(list) == 0 && len(skipped) == 0 && len(errs) == 0 {
		return Result{Output: "no plugins installed (/plugins install <path-or-git-url-or-name@marketplace>)\n"}, nil
	}
	if len(list) > 0 || len(skipped) > 0 {
		b.WriteString("plugins:\n")
	}
	for _, p := range list {
		writePluginRow(&b, p, false)
	}
	for _, name := range skipped {
		// A skipped name may be recorded as the directory or the manifest
		// spelling; display the manifest name like the enabled rows.
		p, err := plugins.LoadPlugin(filepath.Join(root, name))
		if err != nil {
			fmt.Fprintf(&b, "  %-18s (disabled)\n", name)
			continue
		}
		writePluginRow(&b, p, true)
	}
	return Result{Output: b.String()}, nil
}

// writePluginRow renders one /plugins list row — name, version,
// description, contribution counts, an optional (disabled) marker — plus a
// ⚠ line per unsupported manifest key.
func writePluginRow(b *strings.Builder, p plugins.Plugin, disabled bool) {
	marker := ""
	if disabled {
		marker = " (disabled)"
	}
	fmt.Fprintf(b, "  %-18s %-8s %s (%d skills, %d agents, %d commands, %d hooks, %d MCP)%s\n",
		p.Manifest.Name, p.Manifest.Version, p.Manifest.Description,
		len(p.Skills), len(p.Agents), len(p.Commands), hookEntries(p.Hooks), len(p.MCPSpecs), marker)
	for _, key := range p.Ignored {
		fmt.Fprintf(b, "    ⚠ ignored: %s\n", key)
	}
}

// hookEntries counts a hooks configuration's entries across all 13 events.
func hookEntries(cfg hooks.Config) int {
	return len(cfg.PreToolUse) + len(cfg.PostToolUse) + len(cfg.UserPromptSubmit) +
		len(cfg.Stop) + len(cfg.SessionStart) + len(cfg.SessionEnd) +
		len(cfg.SubagentStart) + len(cfg.SubagentStop) + len(cfg.Notification) +
		len(cfg.PreCompact) + len(cfg.PostCompact) + len(cfg.PermissionRequest) +
		len(cfg.PostSampling)
}

// pluginsToggle adds or removes name in the disabled-plugins settings key.
// Both spellings a user can see (manifest or directory name) are accepted;
// disabling stores the name as given, enabling drops every entry that
// resolves to the same plugin.
func (c *Console) pluginsToggle(root string, disable bool, name string) (Result, error) {
	st := c.deps.Settings
	if st == nil {
		return Result{}, fmt.Errorf("settings database is disabled in this session")
	}
	cfg := st.Load()
	if !c.knownPlugin(root, cfg.DisabledPlugins, name) {
		return Result{}, fmt.Errorf("unknown plugin %q (see /plugins list)", name)
	}
	if disable {
		if !slices.Contains(cfg.DisabledPlugins, name) {
			cfg.DisabledPlugins = append(slices.Clone(cfg.DisabledPlugins), name)
		}
	} else {
		cfg.DisabledPlugins = slices.DeleteFunc(cfg.DisabledPlugins, func(d string) bool {
			return d == name || pluginManifestName(root, d) == name
		})
	}
	if err := st.Save(cfg); err != nil {
		return Result{}, err
	}
	verb := "enabled"
	if disable {
		verb = "disabled"
	}
	return Result{Output: fmt.Sprintf("%s %s (takes effect on restart)\n", verb, name)}, nil
}

// knownPlugin reports whether name matches an installed plugin — loaded or
// currently skipped as disabled — by its manifest or directory spelling.
func (c *Console) knownPlugin(root string, disabled []string, name string) bool {
	list, skipped, _ := plugins.LoadWithSkipped(root, disabled)
	for _, p := range list {
		if p.Manifest.Name == name || filepath.Base(p.Path) == name {
			return true
		}
	}
	for _, s := range skipped {
		if s == name || pluginManifestName(root, s) == name {
			return true
		}
	}
	return false
}

// pluginManifestName resolves a plugin directory name to its manifest
// name, falling back to the input when the plugin cannot be loaded (the
// input may already be the manifest spelling).
func pluginManifestName(root, name string) string {
	if p, err := plugins.LoadPlugin(filepath.Join(root, name)); err == nil && p.Manifest.Name != "" {
		return p.Manifest.Name
	}
	return name
}

// disabledPlugins reads the disabled-plugins settings key (nil settings =
// nothing disabled).
func (c *Console) disabledPlugins() []string {
	if c.deps.Settings == nil {
		return nil
	}
	return c.deps.Settings.Load().DisabledPlugins
}
