package claudeenv

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := homeDir
	homeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { homeDir = old })
	return home
}

func writeSkillBody(t *testing.T, home, name, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsListing(t *testing.T) {
	home := fakeHome(t)
	// Missing directory: empty, no error.
	if got, err := Skills(); err != nil || len(got) != 0 {
		t.Fatalf("missing dir = %+v %v", got, err)
	}
	writeSkillBody(t, home, "beta", "---\ndescription: second\n---\nbody")
	writeSkillBody(t, home, "alpha", "---\ndescription: first\n---\nbody")
	writeSkillBody(t, home, "nofm", "no frontmatter here")
	// A plain file (not a skill directory) is skipped.
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink is skipped.
	if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".claude", "skills", "dangling")); err != nil {
		t.Fatal(err)
	}
	got, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("skills = %+v", got)
	}
	if got[0].Name != "alpha" || got[0].Description != "first" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[2].Name != "nofm" || got[2].Description != "" {
		t.Fatalf("nofm = %+v", got[2])
	}
}

func TestParseFrontmatterShapes(t *testing.T) {
	if n, d := parseFrontmatter("def", []byte("plain")); n != "def" || d != "" {
		t.Fatalf("plain = %q %q", n, d)
	}
	if n, d := parseFrontmatter("def", []byte("---\nonly one marker")); n != "def" || d != "" {
		t.Fatalf("partial = %q %q", n, d)
	}
	if n, d := parseFrontmatter("def", []byte("---\ndescription: has desc\nother: x\n---\nbody")); n != "def" || d != "has desc" {
		t.Fatalf("fm = %q %q", n, d)
	}
}

func TestManagedLink(t *testing.T) {
	home := fakeHome(t)
	root := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(root, "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, "links")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "s1"), filepath.Join(dest, "s1")); err != nil {
		t.Fatal(err)
	}
	// Relative symlink into the claude root.
	if err := os.Symlink(filepath.Join("..", ".claude", "skills", "s1"), filepath.Join(dest, "rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(dest, "other")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !managedLink(filepath.Join(dest, "s1"), root) {
		t.Error("absolute managed link not detected")
	}
	if !managedLink(filepath.Join(dest, "rel"), root) {
		t.Error("relative managed link not detected")
	}
	if managedLink(filepath.Join(dest, "other"), root) {
		t.Error("outside link misdetected")
	}
	if managedLink(filepath.Join(dest, "real"), root) {
		t.Error("real dir misdetected")
	}
	if managedLink("", root) || managedLink(filepath.Join(dest, "real"), "") {
		t.Error("empty args must be false")
	}
}

func TestSyncSkillsLifecycle(t *testing.T) {
	home := fakeHome(t)
	writeSkillBody(t, home, "a", "---\ndescription: a\n---\nbody")
	writeSkillBody(t, home, "b", "---\ndescription: b\n---\nbody")
	woca := filepath.Join(home, ".woca")

	linked, err := SyncSkills(woca, []string{"a", "missing", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 2 || linked[0] != "a" || linked[1] != "b" {
		t.Fatalf("linked = %v", linked)
	}
	// Re-sync with only b: a is dropped, b refreshed.
	linked, err = SyncSkills(woca, []string{"b"})
	if err != nil || len(linked) != 1 || linked[0] != "b" {
		t.Fatalf("resync = %v %v", linked, err)
	}
	if _, err := os.Lstat(filepath.Join(woca, "skills", "a")); !os.IsNotExist(err) {
		t.Fatal("deselected link not removed")
	}
	// A real directory owning the name is left alone.
	if err := os.MkdirAll(filepath.Join(woca, "skills", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Remove the link first so the real dir owns the name.
	if err := os.Remove(filepath.Join(woca, "skills", "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(woca, "skills", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncSkills(woca, []string{"b"}); err != nil {
		t.Fatal(err)
	}

	// Unwritable destination fails.
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncSkills(filepath.Join(blocker, "w"), []string{"a"}); err == nil {
		t.Fatal("expected mkdir failure")
	}
}
