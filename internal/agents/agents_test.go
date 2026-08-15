package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadWithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: reviewer\ndescription: Reviews code changes\ntools: read, bash, grep\n---\n# Review\nReview the diff carefully.\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(list))
	}
	d := list[0]
	if d.Name != "reviewer" {
		t.Fatalf("name = %q", d.Name)
	}
	if d.Description != "Reviews code changes" {
		t.Fatalf("description = %q", d.Description)
	}
	wantTools := []string{"read", "bash", "grep"}
	if !reflect.DeepEqual(d.Tools, wantTools) {
		t.Fatalf("tools = %v, want %v", d.Tools, wantTools)
	}
	if !strings.Contains(d.Prompt, "Review the diff carefully.") {
		t.Fatalf("prompt = %q", d.Prompt)
	}
}

func TestLoadWithoutFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "planner.md"), []byte("just the prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(list))
	}
	d := list[0]
	if d.Name != "planner" {
		t.Fatalf("name = %q, want %q", d.Name, "planner")
	}
	if d.Prompt != "just the prompt body" {
		t.Fatalf("prompt = %q", d.Prompt)
	}
	if len(d.Tools) != 0 {
		t.Fatalf("tools = %v, want empty", d.Tools)
	}
}

func TestFind(t *testing.T) {
	list := []Definition{
		{Name: "alpha"},
		{Name: "beta"},
	}
	got, ok := Find(list, "beta")
	if !ok || got.Name != "beta" {
		t.Fatalf("Find(beta) = %+v ok=%v", got, ok)
	}
	if _, ok := Find(list, "missing"); ok {
		t.Fatal("Find(missing) should not be ok")
	}
}

func TestLoadEmptyDir(t *testing.T) {
	list, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 definitions, got %d", len(list))
	}
}

func TestLoadMissingDir(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for missing dir")
	}
}
