package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderRoundTrip(t *testing.T) {
	d := Definition{
		Name:        "reviewer",
		Description: "reviews code",
		Prompt:      "You review code.",
		Tools:       []string{"Read", "Grep"},
	}
	rendered := Render(d)
	if !strings.Contains(rendered, "name: reviewer") || !strings.Contains(rendered, "tools: Read, Grep") {
		t.Fatalf("Render = %q", rendered)
	}
	// Rendered output parses back to the same definition.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Load(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("Load = %+v %v", list, err)
	}
	got := list[0]
	if got.Name != d.Name || got.Description != d.Description || len(got.Tools) != 2 || got.Prompt != d.Prompt {
		t.Fatalf("round trip = %+v", got)
	}

	// Minimal definition without optional fields.
	minimal := Render(Definition{Name: "bare", Prompt: "do things"})
	if strings.Contains(minimal, "description:") || strings.Contains(minimal, "tools:") {
		t.Fatalf("minimal Render = %q", minimal)
	}
}

func TestLoadMissingRootAndNested(t *testing.T) {
	if _, err := Load(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing root should error")
	}
	// A nested directory is walked for definitions.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "helper.md"), []byte("---\nname: helper\ndescription: helps\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Load(root)
	if err != nil || len(list) != 1 || list[0].Name != "helper" {
		t.Fatalf("nested Load = %+v %v", list, err)
	}
}
