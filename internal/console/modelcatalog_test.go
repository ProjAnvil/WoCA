package console

import (
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/wocadb"
)

// seedModelPresets stores catalog rows in the console's settings DB.
func seedModelPresets(t *testing.T, c *Console, presets ...wocadb.ModelPreset) {
	t.Helper()
	db := c.deps.Settings.DB()
	if db == nil {
		t.Fatal("console settings store has no wocadb handle")
	}
	for _, p := range presets {
		if err := db.SaveModelPreset(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelCatalogListAndSwitch(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := t.Context()
	s := &Session{}

	// Without a catalog the legacy hint stays.
	res, err := c.Execute(ctx, "model", nil, s)
	if err != nil || res.Output != "current model=test-model (use /model <name> to switch)\n" {
		t.Fatalf("no-catalog /model = %+v %v", res, err)
	}
	if models := c.Models(); models != nil {
		t.Fatalf("Models() with empty catalog = %+v", models)
	}

	// Seeded presets surface in Models() and the bare /model listing.
	seedModelPresets(t, c,
		wocadb.ModelPreset{ID: "p1", Name: "GLM flagship", Provider: "glm", Model: "glm-5"},
		wocadb.ModelPreset{ID: "p2", Name: "DeepSeek V3", Provider: "deepseek", Model: "deepseek-chat"},
	)
	models := c.Models()
	if len(models) != 2 || models[0].Name != "GLM flagship" || models[1].Model != "deepseek-chat" {
		t.Fatalf("Models() = %+v", models)
	}
	res, err = c.Execute(ctx, "model", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"current model=test-model", "GLM flagship", "glm", "deepseek-chat", "/model <id>"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("catalog listing missing %q:\n%s", want, res.Output)
		}
	}

	// Switching by model id still runs through the ModelFactory.
	switched := 0
	c.deps.ModelFactory = func(name string) (language.ChatModel, error) {
		switched++
		return language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), nil
	}
	if _, err := c.Execute(ctx, "model", []string{"deepseek-chat"}, s); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if switched != 1 || c.deps.Cfg.Model != "deepseek-chat" {
		t.Fatalf("switch did not run the factory: n=%d model=%s", switched, c.deps.Cfg.Model)
	}
	// The current-model marker follows the live configuration.
	res, _ = c.Execute(ctx, "model", nil, s)
	if !strings.Contains(res.Output, "* ") || !strings.Contains(res.Output, "deepseek-chat") {
		t.Fatalf("current marker missing after switch:\n%s", res.Output)
	}
}
