package console

import (
	"fmt"
	"strings"
)

// themeCmd implements /theme (a port of Claude Code's theme switcher): no
// args lists palettes, a name switches the TUI palette live.
func (c *Console) themeCmd(args []string) (Result, error) {
	if len(args) == 0 {
		var b strings.Builder
		b.WriteString("themes (use /theme <name>):\n")
		for _, name := range themeNames {
			fmt.Fprintf(&b, "  %-14s %s\n", name, themeDescriptions[name])
		}
		return Result{Output: b.String()}, nil
	}
	if c.deps.ApplyTheme == nil {
		return Result{}, fmt.Errorf("theme switching is not available in this session (TUI only)")
	}
	if err := c.deps.ApplyTheme(args[0]); err != nil {
		return Result{}, err
	}
	return Result{Output: "theme set to " + args[0] + "\n"}, nil
}

// vimCmd implements /vim: toggle modal (vim) input editing in the TUI.
func (c *Console) vimCmd() Result {
	if c.deps.SetVimModeToggle == nil {
		return Result{Output: "vim input mode is not available in this session (TUI only)\n"}
	}
	if c.deps.SetVimModeToggle() {
		return Result{Output: "vim input mode on (esc = normal · i/a = insert · enter = submit; /vim to disable)\n"}
	}
	return Result{Output: "vim input mode off\n"}
}

// keybindingsCmd implements /keybindings: a reference table of the current
// bindings (input line, TUI, and vim normal mode when enabled).
func (c *Console) keybindingsCmd() Result {
	var b strings.Builder
	b.WriteString("keybindings:\n")
	b.WriteString("  input\n")
	b.WriteString("    enter            send / run command\n")
	b.WriteString("    ctrl+a / ctrl+e  cursor to start / end\n")
	b.WriteString("    ctrl+u           clear input\n")
	b.WriteString("    up / down        history (or command menu)\n")
	b.WriteString("    tab              complete command\n")
	b.WriteString("  session\n")
	b.WriteString("    esc              interrupt running turn (or deny a pending approval)\n")
	b.WriteString("    ctrl+c / ctrl+d  exit\n")
	b.WriteString("    pgup / pgdown    scroll transcript\n")
	b.WriteString("    y / n            approve / deny a pending approval\n")
	b.WriteString("  vim mode (/vim)\n")
	b.WriteString("    i / a / A        insert here / after / at end\n")
	b.WriteString("    h / l / 0 / $    cursor left / right / line start / line end\n")
	b.WriteString("    w / b            next / previous word\n")
	b.WriteString("    x / D / dd       delete char / to end of line / line\n")
	b.WriteString("    esc              back to normal mode\n")
	return Result{Output: b.String()}
}

// themeNames and themeDescriptions mirror the TUI palettes without importing
// the tui package (which imports console).
var themeNames = []string{"dark", "light", "high-contrast"}

var themeDescriptions = map[string]string{
	"dark":          "dark default (cyan prompt, orange accents)",
	"light":         "light palette for bright terminals",
	"high-contrast": "maximum contrast (accessibility)",
}
