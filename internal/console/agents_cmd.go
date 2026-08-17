package console

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/session"
)

// agentsCmd implements /agents, the subagent manager: list definitions, show
// one, create from a template, or delete. Definitions load at startup, so
// structural changes note that a restart applies them.
//
//	/agents                 list
//	/agents show <name>     print the definition file
//	/agents new <name> [d]  create a template in ~/.woca/agents/
//	/agents rm <name>       delete the definition file
func (c *Console) agentsCmd(args []string) (Result, error) {
	defs := c.deps.Agent.AgentDefinitions()
	home := c.deps.AgentsDir
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".woca", "agents")
		} else {
			home = filepath.Join(session.DefaultDir(), "agents")
		}
	}

	if len(args) == 0 || args[0] == "list" {
		if len(defs) == 0 {
			return Result{Output: "no subagent definitions (create one with /agents new <name>)\n"}, nil
		}
		var b strings.Builder
		b.WriteString("subagent definitions:\n")
		for _, d := range defs {
			desc := d.Description
			if desc == "" {
				desc = "(no description)"
			}
			tools := "all tools"
			if len(d.Tools) > 0 {
				tools = fmt.Sprintf("tools: %s", strings.Join(d.Tools, ","))
			}
			fmt.Fprintf(&b, "  %-14s %s  [%s]\n", d.Name, preview(desc, 44), tools)
		}
		return Result{Output: b.String()}, nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "show":
		if len(rest) == 0 {
			return Result{}, fmt.Errorf("usage: /agents show <name>")
		}
		d, ok := agents.Find(defs, rest[0])
		if !ok {
			return Result{}, fmt.Errorf("no subagent %q (see /agents)", rest[0])
		}
		return Result{Output: fmt.Sprintf("# %s\n%s\n", d.Source, agents.Render(d))}, nil

	case "new":
		if len(rest) == 0 {
			return Result{}, fmt.Errorf("usage: /agents new <name> [description]")
		}
		name := sanitizeSessionName(rest[0])
		if name == "" {
			return Result{}, fmt.Errorf("invalid subagent name %q", rest[0])
		}
		path := filepath.Join(home, name+".md")
		if _, err := os.Stat(path); err == nil {
			return Result{}, fmt.Errorf("%s already exists (/agents rm %s first)", path, name)
		}
		desc := "Custom subagent"
		if len(rest) > 1 {
			desc = strings.Join(rest[1:], " ")
		}
		tmpl := agents.Definition{
			Name:        name,
			Description: desc,
			Prompt:      "You are the " + name + " subagent. " + desc + ".\n\nDo the task you are given and return a concise report.",
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(path, []byte(agents.Render(tmpl)), 0o644); err != nil {
			return Result{}, err
		}
		tmpl.Source = path
		c.deps.Agent.SetAgentDefinitions(append(c.deps.Agent.AgentDefinitions(), tmpl))
		return Result{Output: fmt.Sprintf("created %s — available immediately; edit the file and restart to refine\n", path)}, nil

	case "rm", "delete":
		if len(rest) == 0 {
			return Result{}, fmt.Errorf("usage: /agents rm <name>")
		}
		d, ok := agents.Find(defs, rest[0])
		if !ok {
			return Result{}, fmt.Errorf("no subagent %q (see /agents)", rest[0])
		}
		if d.Source == "" {
			return Result{}, fmt.Errorf("definition %q has no source file", rest[0])
		}
		if err := os.Remove(d.Source); err != nil {
			return Result{}, err
		}
		remaining := defs[:0]
		for _, x := range defs {
			if x.Name != d.Name {
				remaining = append(remaining, x)
			}
		}
		c.deps.Agent.SetAgentDefinitions(remaining)
		return Result{Output: fmt.Sprintf("deleted %s (unloaded from this session)\n", d.Source)}, nil

	default:
		return Result{}, fmt.Errorf("unknown /agents subcommand %q (list | show | new | rm)", cmd)
	}
}
