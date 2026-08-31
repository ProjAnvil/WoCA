package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// newCatalogModel builds a TUI model whose console carries a seeded model
// catalog and a counting ModelFactory.
func newCatalogModel(t *testing.T, presets ...wocadb.ModelPreset) (*Model, *int) {
	t.Helper()
	a := newFakeAgent(t, messages.AI("ok"))
	cfg := config.Config{Model: "old-model"}
	st, err := settings.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, p := range presets {
		if err := st.DB().SaveModelPreset(p); err != nil {
			t.Fatal(err)
		}
	}
	switched := 0
	c := console.New(console.Deps{
		Agent:    a,
		Cfg:      &cfg,
		Settings: st,
		ModelFactory: func(name string) (language.ChatModel, error) {
			switched++
			return language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), nil
		},
	})
	m := New(a, nil, cfg)
	m.SetConsole(c)
	return m, &switched
}

// TestModelPickerOpensOnBareModel seeds a catalog and checks that a bare
// /model opens the picker instead of printing the text listing, that the
// fuzzy filter narrows it, and that enter switches through the
// ModelFactory (config updated, transcript confirms).
func TestModelPickerOpensOnBareModel(t *testing.T) {
	m, switched := newCatalogModel(t,
		wocadb.ModelPreset{ID: "p1", Name: "GLM flagship", Provider: "glm", Model: "glm-5"},
		wocadb.ModelPreset{ID: "p2", Name: "DeepSeek V3", Provider: "deepseek", Model: "deepseek-chat"},
	)

	m.input = "/model"
	m = press(m, key("enter"))
	if m.overlay != overlayModels {
		t.Fatalf("bare /model must open the picker, got overlay=%v", m.overlay)
	}
	if len(m.modelsEntries) != 2 {
		t.Fatalf("picker entries = %+v", m.modelsEntries)
	}
	v := m.View()
	if !strings.Contains(v, "switch model") || !strings.Contains(v, "GLM flagship") || !strings.Contains(v, "deepseek") {
		t.Fatalf("picker view missing the catalog:\n%s", v)
	}
	// The current model (old-model) is not in the catalog: no marker row.
	if strings.Contains(v, "* ") {
		t.Fatalf("unexpected current-model marker:\n%s", v)
	}

	// Filter to deepseek and switch.
	for _, r := range "dsk" {
		m = press(m, key(string(r)))
	}
	filtered := m.modelsFiltered()
	if len(filtered) != 1 || filtered[0].Model != "deepseek-chat" {
		t.Fatalf("filtered = %+v", filtered)
	}
	m = press(m, key("enter"))
	if m.overlay != overlayNone {
		t.Fatal("enter must close the picker")
	}
	if *switched != 1 {
		t.Fatalf("ModelFactory calls = %d, want 1", *switched)
	}
	if got := m.console.Cfg().Model; got != "deepseek-chat" {
		t.Fatalf("live config model = %q, want deepseek-chat", got)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "model switched to deepseek-chat") {
		t.Fatalf("transcript missing the switch confirmation:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestModelBareWithoutCatalogFallsBack checks that with no catalog a bare
// /model keeps the console text listing (no empty picker).
func TestModelBareWithoutCatalogFallsBack(t *testing.T) {
	a := newFakeAgent(t, messages.AI("ok"))
	cfg := config.Config{Model: "old-model"}
	c := console.New(console.Deps{Agent: a, Cfg: &cfg})
	m := New(a, nil, cfg)
	m.SetConsole(c)

	m.input = "/model"
	m = press(m, key("enter"))
	if m.overlay != overlayNone {
		t.Fatalf("no catalog must not open a picker, got %v", m.overlay)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "use /model <name> to switch") {
		t.Fatalf("transcript missing the legacy hint:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestModelPickerMarksCurrent checks the current-model marker after a
// switch lands in the catalog.
func TestModelPickerMarksCurrent(t *testing.T) {
	m, _ := newCatalogModel(t,
		wocadb.ModelPreset{ID: "p1", Name: "GLM flagship", Provider: "glm", Model: "glm-5"},
	)
	// Switch first (factory runs through the console command).
	m.input = "/model glm-5"
	m = press(m, key("enter"))
	if m.console.Cfg().Model != "glm-5" {
		t.Fatalf("switch failed: %s", m.console.Cfg().Model)
	}
	m.input = "/model"
	m = press(m, key("enter"))
	if m.overlay != overlayModels {
		t.Fatalf("bare /model must reopen the picker: %v", m.overlay)
	}
	if !strings.Contains(m.View(), "* GLM flagship") {
		t.Fatalf("current-model marker missing:\n%s", m.View())
	}
}
