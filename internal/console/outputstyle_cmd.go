package console

import (
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/prompt"
)

// outputStyleCmd implements /output-style: with no args it lists the styles
// and marks the current one; with a name it rebuilds the live system prompt
// with that style's personality section and persists the choice.
func (c *Console) outputStyleCmd(args []string) (Result, error) {
	current := "default"
	if c.deps.Settings != nil {
		if v := c.deps.Settings.Load().OutputStyle; v != "" {
			current = v
		}
	}
	if len(args) == 0 {
		var b strings.Builder
		b.WriteString("output styles (use /output-style <name>):\n")
		for _, s := range prompt.Styles {
			mark := "  "
			if s.Name == current {
				mark = "▸ "
			}
			fmt.Fprintf(&b, "  %s%-14s %s\n", mark, s.Name, s.Description)
		}
		return Result{Output: b.String()}, nil
	}

	name := strings.ToLower(strings.TrimSpace(args[0]))
	st, ok := prompt.FindStyle(name)
	if !ok {
		return Result{}, fmt.Errorf("unknown output style %q (available: %s)", name, strings.Join(prompt.StyleNames(), ", "))
	}

	sys := prompt.BuildWithStyle(c.deps.Cfg.WorkDir, c.deps.Cfg.ExtraSystemPrompt, c.deps.Cfg.Model, st.Name)
	c.deps.Agent.SetSystemPrompt(sys)
	if c.deps.Settings != nil {
		if st.Name == "default" {
			_ = c.deps.Settings.Clear("output_style")
		} else {
			// Load-modify-Save: a fresh partial struct would clobber the
			// explicitly-persisted bools (e.g. ladym_autoretain) with false.
			cfg := c.deps.Settings.Load()
			cfg.OutputStyle = st.Name
			if err := c.deps.Settings.Save(cfg); err != nil {
				return Result{Output: "style applied for this session (persist failed: " + err.Error() + ")\n"}, nil
			}
		}
	}
	return Result{Output: "output style set to " + st.Name + "\n"}, nil
}
