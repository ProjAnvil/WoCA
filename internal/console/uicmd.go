package console

import (
	"fmt"
	"strings"
)

// themeCmd implements /theme, the theme switcher: no args lists palettes, a
// name switches the TUI palette live.
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
	b.WriteString("    enter            send / run command (while running: queue next)\n")
	b.WriteString("    up (empty line)  take back the queued next message\n")
	b.WriteString("    ctrl+a / ctrl+e  cursor to start / end\n")
	b.WriteString("    ctrl+u           clear input\n")
	b.WriteString("    up / down        history (or command / skill / @agent menu)\n")
	b.WriteString("    tab              complete highlighted menu entry\n")
	b.WriteString("  session\n")
	b.WriteString("    esc esc          interrupt running turn (<300ms, web parity)\n")
	b.WriteString("    ctrl+c           interrupt while running; idle: clear input, twice to exit\n")
	b.WriteString("    ctrl+d           exit\n")
	b.WriteString("    ctrl+o           transcript review (fold/unfold full tool output)\n")
	b.WriteString("    ctrl+k           command palette\n")
	b.WriteString("    ctrl+r           resume a saved session\n")
	b.WriteString("    pgup / pgdown    scroll transcript\n")
	b.WriteString("    y / n            approve / deny a pending approval (enter runs the\n")
	b.WriteString("                     highlighted choice; deny is the default)\n")
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
var themeNames = []string{"dark", "light", "high-contrast", "mono"}

var themeDescriptions = map[string]string{
	"dark":          "dark default (cyan prompt, orange accents)",
	"light":         "light palette for bright terminals",
	"high-contrast": "maximum contrast (accessibility)",
	"mono":          "no chrome color (bold/faint); diff and error reds stay",
}
