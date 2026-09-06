package console

import (
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/keybindings"
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

// keybindingsCmd implements /keybindings: the effective composer bindings
// (defaults plus any ~/.woca/keybindings.json override) followed by a
// reference table of the fixed bindings (input line, TUI, and vim normal
// mode when enabled). `/keybindings open` creates the override file (if
// missing) and reports its path.
func (c *Console) keybindingsCmd(args []string) (Result, error) {
	path, pathErr := c.keybindingsPath()
	if len(args) > 0 && args[0] == "open" {
		if pathErr != nil {
			return Result{}, pathErr
		}
		if err := keybindings.EnsureFile(path); err != nil {
			return Result{}, err
		}
		return Result{Output: "keybindings config: " + path + "\n"}, nil
	}
	km := keybindings.Default()
	var loadErr error
	if pathErr == nil {
		km, loadErr = keybindings.Load(path)
	}
	var b strings.Builder
	b.WriteString("keybindings:\n")
	if loadErr != nil {
		fmt.Fprintf(&b, "  ⚠ %v (using defaults)\n", loadErr)
	}
	b.WriteString("  composer\n")
	fmt.Fprintf(&b, "    ctrl+j           %s\n", describeAction(km, "ctrl+j", keybindings.ActionComposerNewline, "insert a newline without submitting"))
	fmt.Fprintf(&b, "    ctrl+g           %s\n", describeAction(km, "ctrl+g", keybindings.ActionComposerEditor, "open the draft in $VISUAL/$EDITOR"))
	b.WriteString("    shift+tab        cycle the permission mode (web composer parity)\n")
	if pathErr == nil {
		fmt.Fprintf(&b, "    config: %s (/keybindings open to create)\n", path)
	}
	b.WriteString("  input\n")
	b.WriteString("    enter            send / run command (while running: steer this turn)\n")
	b.WriteString("    ctrl+a / ctrl+e  cursor to start / end\n")
	b.WriteString("    ctrl+u           clear input\n")
	b.WriteString("    up / down        history (or command / skill / @agent menu)\n")
	b.WriteString("    tab              complete highlighted menu entry\n")
	b.WriteString("  session\n")
	b.WriteString("    esc esc          interrupt running turn; with a pending steer, resubmits\n")
	b.WriteString("                     it immediately as a fresh turn instead of losing it\n")
	b.WriteString("    ctrl+c           interrupt while running (same steer resubmit); idle: clear input, twice to exit\n")
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
	return Result{Output: b.String()}, nil
}

// keybindingsPath resolves the config file /keybindings reads and writes:
// c.deps.KeybindingsPath when set (tests), otherwise ~/.woca/keybindings.json.
func (c *Console) keybindingsPath() (string, error) {
	if c.deps.KeybindingsPath != "" {
		return c.deps.KeybindingsPath, nil
	}
	return keybindings.DefaultPath()
}

// describeAction reports a bound key's action, noting when it has been
// overridden away from its documented default action.
func describeAction(km keybindings.Map, key, wantAction, defaultDesc string) string {
	if km.Action(key) == wantAction {
		return defaultDesc
	}
	if action := km.Action(key); action != "" {
		return "remapped to " + action
	}
	return "unbound (default: " + defaultDesc + ")"
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
