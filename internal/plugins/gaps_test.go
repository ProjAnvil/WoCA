package plugins

import (
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// skipIfRoot guards permission-denied fixtures.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denied fixtures are ineffective")
	}
}

// runGit executes git quietly, failing the test on error.
func runGit(t *testing.T, args ...string) {
	t.Helper()
	args = append([]string{"-c", "user.email=test@example.com", "-c", "user.name=test"}, args...)
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// gitServer serves a temp tree of bare git repositories over smart HTTP
// (git http-backend behind net/http/cgi) and returns its base URL. Repos:
// good.git carries a plugin.json; empty.git does not.
func gitServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Isolate git from the user's global configuration.
	t.Setenv("HOME", t.TempDir())

	root := t.TempDir()
	mkRepo := func(name string, manifest bool) {
		work := filepath.Join(root, name+"-work")
		bare := filepath.Join(root, name+".git")
		runGit(t, "init", "-q", "--bare", bare)
		runGit(t, "init", "-q", work)
		if manifest {
			if err := os.MkdirAll(filepath.Join(work, "skills", "deploy"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "plugin.json"),
				[]byte(`{"name":"`+name+`","version":"1.0.0"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "skills", "deploy", "SKILL.md"),
				[]byte("---\nname: deploy\ndescription: d\n---\nGo.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(work, "README"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, "-C", work, "add", "-A")
		runGit(t, "-C", work, "commit", "-qm", "init")
		runGit(t, "-C", work, "push", "-q", bare, "HEAD")
	}
	mkRepo("good", true)
	mkRepo("empty", false)

	srv := httptest.NewServer(&cgi.Handler{
		Path: git,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_HTTP_EXPORT_ALL=1", "GIT_PROJECT_ROOT=" + root},
	})
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestInstallFromGitURL(t *testing.T) {
	base := gitServer(t)
	dstRoot := filepath.Join(t.TempDir(), "plugins")

	path, err := Install(base+"/good.git", dstRoot)
	if err != nil {
		t.Fatalf("install from git: %v", err)
	}
	if filepath.Base(path) != "good" {
		t.Fatalf("installed at %s", path)
	}
	// The clone's .git metadata is stripped.
	if _, err := os.Stat(filepath.Join(path, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git left behind after install (err=%v)", err)
	}
	p, err := LoadPlugin(path)
	if err != nil || p.Manifest.Name != "good" || len(p.Skills) != 1 {
		t.Fatalf("installed plugin = %+v err=%v", p, err)
	}

	// A repo without a manifest clones but fails validation and is cleaned up.
	if _, err := Install(base+"/empty.git", dstRoot); err == nil {
		t.Fatal("expected validation failure for a manifest-less repo")
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "empty")); !os.IsNotExist(err) {
		t.Fatalf("failed install not cleaned up (err=%v)", err)
	}
}

// Removing a previous installation requires write access to the root.
func TestInstallGitRemoveAllError(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })

	if _, err := Install("http://127.0.0.1:1/repo.git", root); err == nil {
		t.Fatal("Install accepted an unremovable previous installation")
	}
}

func TestDefaultRootWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if got := DefaultRoot(); got != "" {
		t.Fatalf("DefaultRoot without HOME = %q, want empty", got)
	}
}

func TestLoadPluginMissingManifestAndDefaultName(t *testing.T) {
	if _, err := LoadPlugin(t.TempDir()); err == nil {
		t.Fatal("LoadPlugin accepted a directory without plugin.json")
	}

	dir := filepath.Join(t.TempDir(), "nameless")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"version":"1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "nameless" {
		t.Fatalf("manifest name = %q, want directory base", p.Manifest.Name)
	}
}

func TestLoadRootIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, errs := Load(f)
	if list != nil || len(errs) != 1 {
		t.Fatalf("Load(file) = %v, %v; want nil, one error", list, errs)
	}
}

func TestInstallCopyTreeUnwalkableSource(t *testing.T) {
	skipIfRoot(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Searchable but unreadable: the manifest stat passes, the walk fails.
	if err := os.Chmod(src, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(src, 0o755) })

	if _, err := Install(src, filepath.Join(t.TempDir(), "plugins")); err == nil {
		t.Fatal("Install accepted an unreadable source tree")
	}
}

func TestCopyTreeDirectErrors(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	if err := copyTree(filepath.Join(t.TempDir(), "missing"), dst); err == nil {
		t.Fatal("copyTree accepted a missing source")
	}
	file := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(file, dst); err == nil {
		t.Fatal("copyTree accepted a non-directory source")
	}
}

func TestCopyTreeUnreadableFile(t *testing.T) {
	skipIfRoot(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "secret"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "secret"), 0o644) })
	if err := copyTree(src, filepath.Join(t.TempDir(), "dst")); err == nil {
		t.Fatal("copyTree accepted an unreadable file")
	}
}
