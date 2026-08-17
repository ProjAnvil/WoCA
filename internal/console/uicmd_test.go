package console

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestThemeCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)

	// No args: palette listing.
	res, err := c.Execute(context.Background(), "theme", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dark", "light", "high-contrast"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("theme list missing %q:\n%s", want, res.Output)
		}
	}

	// Without a TUI hook, switching degrades with a helpful error.
	if _, err := c.Execute(context.Background(), "theme", []string{"dark"}, &Session{}); err == nil ||
		!strings.Contains(err.Error(), "not available") {
		t.Fatalf("theme without ApplyTheme = %v", err)
	}

	// With the hook wired the palette is applied live.
	applied := ""
	c.deps.ApplyTheme = func(name string) error { applied = name; return nil }
	res, err = c.Execute(context.Background(), "theme", []string{"light"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if applied != "light" || !strings.Contains(res.Output, "theme set to light") {
		t.Fatalf("apply = %q out=%q", applied, res.Output)
	}

	// A failing hook surfaces its error.
	c.deps.ApplyTheme = func(name string) error { return errors.New("no such palette") }
	if _, err := c.Execute(context.Background(), "theme", []string{"neon"}, &Session{}); err == nil {
		t.Fatal("ApplyTheme error should surface")
	}
}

func TestVimCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)

	// Without the TUI toggle: helpful message, not an error.
	res, err := c.Execute(context.Background(), "vim", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "not available") {
		t.Fatalf("vim without toggle = %q, %v", res.Output, err)
	}

	// Each call flips the mode and reports the new state.
	on := false
	c.deps.SetVimModeToggle = func() bool { on = !on; return on }
	res, err = c.Execute(context.Background(), "vim", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "vim input mode on") {
		t.Fatalf("vim on = %q, %v", res.Output, err)
	}
	res, err = c.Execute(context.Background(), "vim", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "vim input mode off") {
		t.Fatalf("vim off = %q, %v", res.Output, err)
	}
}

func TestKeybindingsCommand(t *testing.T) {
	c, _, _ := newTestConsole(t, nil)
	res, err := c.Execute(context.Background(), "keybindings", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"keybindings:", "input", "session", "vim mode", "esc"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("keybindings missing %q:\n%s", want, res.Output)
		}
	}
}
