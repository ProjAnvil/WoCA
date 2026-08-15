package console

import (
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/plugins"
)

// pluginsCmd implements /plugins: list installed plugins, install from a
// local path or git URL, or remove one. Installed agents merge into the live
// session; skills load on restart (the Skill tool binds at startup).
func (c *Console) pluginsCmd(args []string) (Result, error) {
	root := c.deps.PluginsRoot
	if root == "" {
		root = plugins.DefaultRoot()
	}
	if len(args) == 0 || args[0] == "list" {
		list, errs := plugins.Load(root)
		var b strings.Builder
		for _, err := range errs {
			fmt.Fprintf(&b, "  ⚠ %v\n", err)
		}
		if len(list) == 0 && len(errs) == 0 {
			return Result{Output: "no plugins installed (/plugins install <path-or-git-url>)\n"}, nil
		}
		if len(list) > 0 {
			b.WriteString("plugins:\n")
		}
		for _, p := range list {
			fmt.Fprintf(&b, "  %-18s %-8s %s (%d skills, %d agents)\n",
				p.Manifest.Name, p.Manifest.Version, p.Manifest.Description, len(p.Skills), len(p.Agents))
		}
		return Result{Output: b.String()}, nil
	}

	switch args[0] {
	case "install":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /plugins install <path-or-git-url>")
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
		if len(p.Skills) > 0 {
			out += fmt.Sprintf(" (%d skills load on restart)", len(p.Skills))
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

	default:
		return Result{}, fmt.Errorf("unknown /plugins subcommand %q (list | install | rm)", args[0])
	}
}
