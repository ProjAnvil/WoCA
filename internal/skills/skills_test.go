package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndParse(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "commit")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: commit\ndescription: Write a good commit message\n---\n# Commit\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(list))
	}
	s := list[0]
	if s.Name != "commit" || s.Description != "Write a good commit message" {
		t.Fatalf("skill = %+v", s)
	}
	if !strings.Contains(s.Body, "Do the thing.") {
		t.Fatalf("body = %q", s.Body)
	}

	got, ok := Find(list, "commit")
	if !ok || got.Name != "commit" {
		t.Fatalf("Find = %+v ok=%v", got, ok)
	}
	if _, ok := Find(list, "missing"); ok {
		t.Fatal("Find(missing) should not be ok")
	}
}

func TestParseWithoutFrontmatter(t *testing.T) {
	s := parseSkill("myname", []byte("just body text"))
	if s.Name != "myname" {
		t.Fatalf("name = %q", s.Name)
	}
	if s.Body != "just body text" {
		t.Fatalf("body = %q", s.Body)
	}
}

func TestParseArgsCommaSeparated(t *testing.T) {
	s := parseSkill("myname", []byte("---\nargs: foo, bar, baz\n---\nbody"))
	if len(s.Args) != 3 {
		t.Fatalf("args = %+v", s.Args)
	}
	if s.Args[0].Name != "foo" || s.Args[1].Name != "bar" || s.Args[2].Name != "baz" {
		t.Fatalf("args = %+v", s.Args)
	}
	for _, a := range s.Args {
		if a.Description != "" {
			t.Fatalf("arg %q has unexpected description %q", a.Name, a.Description)
		}
	}
}

func TestParseArgsList(t *testing.T) {
	content := "---\nname: review\nargs:\n  - files: files to review\n  - depth: how deep to go\n---\nbody"
	s := parseSkill("myname", []byte(content))
	if len(s.Args) != 2 {
		t.Fatalf("args = %+v", s.Args)
	}
	if s.Args[0].Name != "files" || s.Args[0].Description != "files to review" {
		t.Fatalf("args[0] = %+v", s.Args[0])
	}
	if s.Args[1].Name != "depth" || s.Args[1].Description != "how deep to go" {
		t.Fatalf("args[1] = %+v", s.Args[1])
	}
}

func TestParseNoArgs(t *testing.T) {
	s := parseSkill("myname", []byte("---\nname: plain\n---\nbody"))
	if s.Args != nil {
		t.Fatalf("Args should be nil for files without args, got %+v", s.Args)
	}
}

func TestExpand(t *testing.T) {
	s := Skill{
		Name: "greet",
		Body: "Hello {name}, you are {role}. Keep {name}.",
	}
	got := Expand(s, map[string]any{"name": "Ada", "role": 42})
	want := "Hello Ada, you are 42. Keep Ada."
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}

func TestExpandUnknownPlaceholder(t *testing.T) {
	s := Skill{Body: "Hello {name} and {unknown}."}
	got := Expand(s, map[string]any{"name": "Ada"})
	want := "Hello Ada and {unknown}."
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}

func TestLoaderHotReload(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "task")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: task\ndescription: v1\n---\nbody v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(dir)

	list, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Description != "v1" || !strings.Contains(list[0].Body, "body v1") {
		t.Fatalf("first load = %+v", list)
	}

	if err := os.WriteFile(path, []byte("---\nname: task\ndescription: v2\n---\nbody v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err = l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Description != "v2" || !strings.Contains(list[0].Body, "body v2") {
		t.Fatalf("second load = %+v", list)
	}
}
