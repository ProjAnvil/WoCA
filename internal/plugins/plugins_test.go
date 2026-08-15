package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// makePlugin writes a plugin tree with one skill and one agent definition.
func makePlugin(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	mkdir := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{dir}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkdir()
	manifest := `{"name":"` + name + `","version":"1.2.3","description":"test plugin"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir("skills", "deploy")
	skill := "---\nname: deploy\ndescription: Deploys things\n---\nRun the deploy.\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", "deploy", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir("agents")
	agent := "---\nname: deployer\ndescription: Deploys code\n---\nYou deploy code.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "deployer.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadPlugin(t *testing.T) {
	src := makePlugin(t, t.TempDir(), "acme")
	p, err := LoadPlugin(src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "acme" || p.Manifest.Version != "1.2.3" {
		t.Fatalf("manifest = %+v", p.Manifest)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "acme-deploy" {
		t.Fatalf("skills = %+v (want namespaced acme-deploy)", p.Skills)
	}
	if len(p.Agents) != 1 || p.Agents[0].Name != "deployer" {
		t.Fatalf("agents = %+v", p.Agents)
	}
}

func TestLoadRootSkipsNonPlugins(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "good")
	// A stray non-plugin directory and a dot-dir.
	os.MkdirAll(filepath.Join(root, "stray"), 0o755)
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)

	list, errs := Load(root)
	if len(list) != 1 || list[0].Manifest.Name != "good" {
		t.Fatalf("list = %+v", list)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v (want the stray dir reported)", errs)
	}
}

func TestLoadMissingRoot(t *testing.T) {
	list, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if list != nil || errs != nil {
		t.Fatalf("missing root should be quiet: %v %v", list, errs)
	}
}

func TestInstallFromLocalPath(t *testing.T) {
	srcRoot := t.TempDir()
	src := makePlugin(t, srcRoot, "acme")
	dstRoot := filepath.Join(t.TempDir(), "plugins")

	path, err := Install(src, dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "acme" {
		t.Fatalf("installed at %s", path)
	}
	p, err := LoadPlugin(path)
	if err != nil || len(p.Skills) != 1 || len(p.Agents) != 1 {
		t.Fatalf("installed plugin = %+v err=%v", p, err)
	}
	// Reinstall replaces cleanly.
	if _, err := Install(src, dstRoot); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
}

func TestInstallRejectsMissingManifest(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "README"), []byte("not a plugin"), 0o644)
	if _, err := Install(src, filepath.Join(t.TempDir(), "plugins")); err == nil {
		t.Fatal("install without plugin.json should fail")
	}
}

func TestUninstall(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "acme")
	if err := Uninstall("acme", root); err != nil {
		t.Fatal(err)
	}
	list, _ := Load(root)
	if len(list) != 0 {
		t.Fatalf("uninstall left %d plugins", len(list))
	}
	if err := Uninstall("../escape", root); err == nil {
		t.Fatal("path separators in plugin names must be rejected")
	}
}
