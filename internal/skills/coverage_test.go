package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadNestedAndUnreadable(t *testing.T) {
	root := t.TempDir()
	// Nested skill directories are walked; the directory name is the
	// default skill name.
	nested := filepath.Join(root, "group", "inner")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte("---\nname: inner\ndescription: d\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Load(root)
	if err != nil || len(list) != 1 || list[0].Name != "inner" {
		t.Fatalf("Load = %+v %v", list, err)
	}

	// A missing root yields an empty list without error (walk skips it).
	if list, err := Load(filepath.Join(root, "missing")); err != nil || len(list) != 0 {
		t.Fatalf("missing root = %+v %v", list, err)
	}

	// An entry whose stat fails (dangling symlink) is skipped.
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
}

func TestSkillFrontmatterVariants(t *testing.T) {
	root := t.TempDir()
	// YAML argument list; entries without a colon are skipped.
	if err := os.MkdirAll(filepath.Join(root, "listy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "listy", "SKILL.md"), []byte(`---
name: listy
description: has args
args:
  - target: where to look
  - pattern: what to match
  - nodesc
---
body`), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Load(root)
	if err != nil || len(list) != 1 {
		t.Fatalf("Load = %+v %v", list, err)
	}
	args := list[0].Args
	if len(args) != 2 || args[0].Name != "target" || args[1].Description != "what to match" {
		t.Fatalf("args = %+v", args)
	}
}
