// Package keybindings loads a user-overridable action map for the TUI
// composer. Bindings resolve a normalized key string (as reported by
// bubbletea's tea.KeyMsg.String()) to a named action; unknown or malformed
// overrides are rejected as a whole so a bad config file never makes the
// TUI unusable.
package keybindings

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// Known composer actions. Load and Apply reject overrides to any other
// action name.
const (
	ActionComposerNewline = "composer:newline"
	ActionComposerEditor  = "composer:editor"
)

var knownActions = map[string]bool{
	ActionComposerNewline: true,
	ActionComposerEditor:  true,
}

// Map resolves a normalized key string to its bound action.
type Map struct {
	bindings map[string]string
}

// Default returns woca's built-in bindings: ctrl+j inserts a newline and
// ctrl+g opens the draft in $VISUAL/$EDITOR.
func Default() Map {
	return Map{bindings: map[string]string{
		"ctrl+j": ActionComposerNewline,
		"ctrl+g": ActionComposerEditor,
	}}
}

// Action returns the action bound to key, or "" when the key is unbound.
func (m Map) Action(key string) string {
	if m.bindings == nil {
		return ""
	}
	return m.bindings[normalize(key)]
}

// Bindings returns a copy of the effective key->action map, for display.
func (m Map) Bindings() map[string]string {
	out := make(map[string]string, len(m.bindings))
	maps.Copy(out, m.bindings)
	return out
}

// Apply merges overrides into m, replacing the action bound to each key.
// Overrides are assumed already validated (see Load).
func (m *Map) Apply(overrides map[string]string) {
	if m.bindings == nil {
		m.bindings = map[string]string{}
	}
	for k, v := range overrides {
		m.bindings[normalize(k)] = v
	}
}

func normalize(key string) string { return strings.ToLower(strings.TrimSpace(key)) }

// Load reads a keybindings JSON file (a flat key->action object) at path and
// applies validated overrides onto the defaults. A missing file yields the
// defaults with no error. A malformed file — invalid JSON or an unknown
// action — is rejected as a whole: Load returns the untouched defaults
// alongside a descriptive error.
func Load(path string) (Map, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return Default(), err
	}
	var overrides map[string]string
	if err := json.Unmarshal(data, &overrides); err != nil {
		return Default(), fmt.Errorf("keybindings: parse %s: %w", path, err)
	}
	for key, action := range overrides {
		if !knownActions[action] {
			return Default(), fmt.Errorf("keybindings: %s: unknown action %q for key %q", path, action, key)
		}
	}
	m := Default()
	m.Apply(overrides)
	return m, nil
}

// DefaultPath returns ~/.woca/keybindings.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".woca", "keybindings.json"), nil
}

// EnsureFile creates an empty overrides file at path (and its parent
// directory) if it does not already exist. It is idempotent.
func EnsureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{}\n"), 0o644)
}
