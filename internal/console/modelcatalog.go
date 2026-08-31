package console

import (
	"fmt"
	"strings"
)

// /model catalog: the operator-authored model presets from the settings DB
// (wocadb.ModelPreset — the same rows the web UI reads through
// GET /api/models) offered as a pick list. Selection still goes through the
// existing ModelFactory switch by model id; presets only add the shared
// catalog, not a second switching path.

// ModelEntry is one catalog model offered for /model selection.
type ModelEntry struct {
	Name     string // operator-authored display name
	Provider string
	Model    string // the id /model switches with
}

// Models lists the model catalog (newest first). Nil settings, no DB, an
// empty catalog, or a read failure all yield nil — the free-string /model
// behavior is the fallback, never an error surface.
func (c *Console) Models() []ModelEntry {
	if c.deps.Settings == nil || c.deps.Settings.DB() == nil {
		return nil
	}
	presets, err := c.deps.Settings.DB().ListModelPresets()
	if err != nil || len(presets) == 0 {
		return nil
	}
	out := make([]ModelEntry, 0, len(presets))
	for _, p := range presets {
		out = append(out, ModelEntry{Name: p.Name, Provider: p.Provider, Model: p.Model})
	}
	return out
}

// modelCatalogText renders the catalog listing for /model with no argument
// (the line REPL's picker; the TUI opens its overlay picker instead).
func (c *Console) modelCatalogText() string {
	entries := c.Models()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "current model=%s\ncatalog (pick with /model <id>):\n", c.deps.Cfg.Model)
	for _, e := range entries {
		marker := " "
		if e.Model == c.deps.Cfg.Model {
			marker = "*"
		}
		fmt.Fprintf(&b, " %s  %-20s %-10s %s\n", marker, e.Name, e.Provider, e.Model)
	}
	return b.String()
}
