package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// --- helpers ---------------------------------------------------------------

// newTestStore returns a store over a fresh root with a generous cap.
func newTestStore(t *testing.T) (*WorkspaceStore, string) {
	t.Helper()
	root := t.TempDir()
	return NewWorkspaceStore(root, 64*1024*1024), root
}

func mustWorkspaceDir(t *testing.T, s *WorkspaceStore, id string) string {
	t.Helper()
	dir, err := s.Dir(id)
	if err != nil {
		t.Fatalf("Dir(%q): %v", id, err)
	}
	return dir
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", p, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink(%s -> %s): %v", link, target, err)
	}
}

// treeEntry is one entry of a host directory tree.
type treeEntry struct {
	kind byte // 'f' file, 'd' dir, 'l' symlink
	data string
	link string
}

// readHostTree snapshots root as a map of slash-relative paths to entries
// (root itself excluded). Symlinks are not followed.
func readHostTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			out[name] = treeEntry{kind: 'd'}
		case d.Type()&os.ModeSymlink != 0:
			link, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			out[name] = treeEntry{kind: 'l', link: link}
		default:
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[name] = treeEntry{kind: 'f', data: string(data)}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func assertTree(t *testing.T, root string, want map[string]treeEntry) {
	t.Helper()
	got := readHostTree(t, root)
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("tree %s: missing %q (want %c)", root, name, w.kind)
			continue
		}
		if g != w {
			t.Errorf("tree %s: %q = %+v, want %+v", root, name, g, w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("tree %s: unexpected extra entry %q = %+v", root, name, got[name])
		}
	}
}

// archEntry is one entry of a container /workspace archive.
type archEntry struct {
	typeflag byte
	data     string
	link     string
}

// readContainerWorkspace pulls the container's /workspace via CopyOut and
// returns its entries keyed by workspace-relative name (the Docker-style
// top-level "workspace/" prefix is stripped).
func readContainerWorkspace(t *testing.T, c Container) map[string]archEntry {
	t.Helper()
	rc, err := c.CopyOut(context.Background(), "/workspace")
	if err != nil {
		t.Fatalf("CopyOut(/workspace): %v", err)
	}
	defer func() { _ = rc.Close() }()

	out := map[string]archEntry{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		name := path.Clean(hdr.Name)
		if name == "." || name == "workspace" || name == "/" {
			continue
		}
		name = strings.TrimPrefix(name, "workspace/")
		e := archEntry{typeflag: hdr.Typeflag, link: hdr.Linkname}
		if hdr.Typeflag == tar.TypeReg {
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				t.Fatalf("tar read %s: %v", name, rerr)
			}
			e.data = string(data)
		}
		out[name] = e
	}
}

// execScript runs sh -c <script> in the container and requires exit 0.
func execScript(t *testing.T, c Container, script string) {
	t.Helper()
	res := mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", script}})
	if res.ExitCode != 0 {
		t.Fatalf("sh -c %q: exit %d, stderr %q", script, res.ExitCode, res.Stderr)
	}
}

// containerWorkspaceHostPath returns the host path backing a fake
// container's /workspace: fake-container commands see host paths, so
// in-container scripts must be written against this path.
func containerWorkspaceHostPath(t *testing.T, c Container) string {
	t.Helper()
	fc, ok := c.(*FakeContainer)
	if !ok {
		t.Fatalf("container is %T, want *FakeContainer", c)
	}
	return filepath.Join(fc.dir, "workspace")
}

// --- ValidWorkspaceID ------------------------------------------------------

func TestValidWorkspaceID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"a", true},
		{"ws1", true},
		{"A-b_2", true},
		{"deep--name__42", true},
		{"----", true},
		{"____", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../etc", false},
		{"/abs", false},
		{"ws id", false},
		{" ", false},
		{"a.b", false},
		{".git", false},
		{"a/b", false},
		{"a\\b", false},
		{"a:b", false},
		{"café", false},
		{"ws\n", false},
	}
	for _, tt := range tests {
		if got := ValidWorkspaceID(tt.id); got != tt.want {
			t.Errorf("ValidWorkspaceID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// --- Dir -------------------------------------------------------------------

func TestWorkspaceStore_Dir(t *testing.T) {
	s, root := newTestStore(t)

	dir, err := s.Dir("proj-1")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want := filepath.Join(root, "proj-1"); dir != want {
		t.Errorf("Dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("Dir did not create a directory: %v", err)
	}
	// Idempotent on repeat calls.
	if _, err := s.Dir("proj-1"); err != nil {
		t.Fatalf("second Dir: %v", err)
	}

	for _, bad := range []string{"", "../etc", "/abs", "ws id", ".git"} {
		if _, err := s.Dir(bad); err == nil {
			t.Errorf("Dir(%q) succeeded, want error", bad)
		}
	}
	names := listDirNames(t, root)
	if len(names) != 1 || names[0] != "proj-1" {
		t.Errorf("root after invalid Dir calls = %v, want [proj-1]", names)
	}
}

// --- Upload ----------------------------------------------------------------

func TestWorkspaceStore_UploadSkipsGitAndCopiesContent(t *testing.T) {
	s, _ := newTestStore(t)
	dir := mustWorkspaceDir(t, s, "ws1")
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main")
	writeTestFile(t, filepath.Join(dir, "sub", "util.txt"), "utils")
	writeTestFile(t, filepath.Join(dir, ".git", "config"), "[core]")
	writeTestFile(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh")

	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	if err := s.Upload(context.Background(), c, "ws1"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	entries := readContainerWorkspace(t, c)
	for name := range entries {
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			t.Errorf("container archive contains .git entry %q", name)
		}
	}
	want := map[string]archEntry{
		"main.go":      {typeflag: tar.TypeReg, data: "package main"},
		"sub":          {typeflag: tar.TypeDir},
		"sub/util.txt": {typeflag: tar.TypeReg, data: "utils"},
	}
	for name, w := range want {
		if got, ok := entries[name]; !ok {
			t.Errorf("container archive missing %q", name)
		} else if got != w {
			t.Errorf("container archive %q = %+v, want %+v", name, got, w)
		}
	}
}

func TestWorkspaceStore_UploadSymlinkPolicy(t *testing.T) {
	// The link sits at the workspace root, so targets resolve against ".".
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"in-root file", "main.go", false},
		{"in-root deeper", "sub/inner.txt", false},
		{"in-root via dotdot", "sub/../main.go", false},
		{"parent itself", "..", true},
		{"dotdot escape", "../outside", true},
		{"deep dotdot escape", "sub/../../outside", true},
		{"absolute", "/etc/passwd", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			dir := mustWorkspaceDir(t, s, "ws1")
			writeTestFile(t, filepath.Join(dir, "main.go"), "m")
			writeTestFile(t, filepath.Join(dir, "sub", "inner.txt"), "i")
			mustSymlink(t, tt.target, filepath.Join(dir, "link"))

			be := newBackend(t)
			c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

			err := s.Upload(context.Background(), c, "ws1")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Upload with link %q succeeded, want error", tt.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("Upload with link %q: %v", tt.target, err)
			}
			got, ok := readContainerWorkspace(t, c)["link"]
			if !ok || got.typeflag != tar.TypeSymlink || got.link != tt.target {
				t.Fatalf("container archive link = %+v, want symlink -> %q", got, tt.target)
			}
		})
	}
}

func TestWorkspaceStore_UploadSizeCap(t *testing.T) {
	s := NewWorkspaceStore(t.TempDir(), 1024)
	dir := mustWorkspaceDir(t, s, "ws1")
	writeTestFile(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 2048))

	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	err := s.Upload(context.Background(), c, "ws1")
	if !errors.Is(err, ErrWorkspaceTooLarge) {
		t.Fatalf("Upload over cap: got %v, want ErrWorkspaceTooLarge", err)
	}
}

func TestWorkspaceStore_UploadInvalidID(t *testing.T) {
	s, _ := newTestStore(t)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})
	if err := s.Upload(context.Background(), c, "../etc"); err == nil {
		t.Fatal("Upload with invalid id succeeded, want error")
	}
}

// --- Sync ------------------------------------------------------------------

func TestWorkspaceStore_SyncRoundTrip(t *testing.T) {
	s, root := newTestStore(t)
	dir := mustWorkspaceDir(t, s, "ws1")
	writeTestFile(t, filepath.Join(dir, "main.go"), "v1")
	writeTestFile(t, filepath.Join(dir, "data.bin"), "payload")
	writeTestFile(t, filepath.Join(dir, "sub", "nested.txt"), "nested")
	mustSymlink(t, "main.go", filepath.Join(dir, "link"))

	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	if err := s.Upload(context.Background(), c, "ws1"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Mutate the container-side workspace: edit, delete, create, new dir.
	// Fake-container commands see host paths, so address the host directory
	// backing /workspace.
	wsHost := containerWorkspaceHostPath(t, c)
	execScript(t, c, fmt.Sprintf(`printf v2 > '%s/main.go' && rm '%s/data.bin' &&
		echo new > '%s/created.txt' && mkdir '%s/newdir'`, wsHost, wsHost, wsHost, wsHost))

	if err := s.Sync(context.Background(), c, "ws1"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	assertTree(t, filepath.Join(root, "ws1"), map[string]treeEntry{
		"main.go":        {kind: 'f', data: "v2"},
		"sub":            {kind: 'd'},
		"sub/nested.txt": {kind: 'f', data: "nested"},
		"link":           {kind: 'l', link: "main.go"},
		"created.txt":    {kind: 'f', data: "new\n"},
		"newdir":         {kind: 'd'},
		// data.bin was deleted inside the container: gone after Sync.
	})

	// No staging or replaced-dir leftovers next to the workspace.
	if names := listDirNames(t, root); len(names) != 1 || names[0] != "ws1" {
		t.Errorf("root after Sync = %v, want exactly [ws1]", names)
	}
}

func TestWorkspaceStore_SyncFailureKeepsOldDirectory(t *testing.T) {
	tests := []struct {
		name     string
		cap      int64
		sabotage func(t *testing.T, c Container)
		wantErr  error
	}{
		{
			name: "copy out fails",
			sabotage: func(t *testing.T, c Container) {
				if err := c.Remove(context.Background(), true); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
		},
		{
			name: "escaping symlink from container",
			sabotage: func(t *testing.T, c Container) {
				ws := containerWorkspaceHostPath(t, c)
				execScript(t, c, fmt.Sprintf("ln -s ../../escape '%s/bad'", ws))
			},
		},
		{
			name: "absolute symlink from container",
			sabotage: func(t *testing.T, c Container) {
				ws := containerWorkspaceHostPath(t, c)
				execScript(t, c, fmt.Sprintf("ln -s /etc/passwd '%s/bad'", ws))
			},
		},
		{
			name: "archive over cap",
			cap:  8192,
			sabotage: func(t *testing.T, c Container) {
				ws := containerWorkspaceHostPath(t, c)
				execScript(t, c, fmt.Sprintf("head -c 32768 /dev/zero > '%s/big.bin'", ws))
			},
			wantErr: ErrWorkspaceTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := int64(64 * 1024 * 1024)
			if tt.cap != 0 {
				cap = tt.cap
			}
			s := NewWorkspaceStore(t.TempDir(), cap)
			dir := mustWorkspaceDir(t, s, "ws1")
			writeTestFile(t, filepath.Join(dir, "keep.txt"), "keep")
			writeTestFile(t, filepath.Join(dir, "sub", "b.txt"), "b")

			be := newBackend(t)
			c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})
			if err := s.Upload(context.Background(), c, "ws1"); err != nil {
				t.Fatalf("Upload: %v", err)
			}

			tt.sabotage(t, c)
			err := s.Sync(context.Background(), c, "ws1")
			if err == nil {
				t.Fatal("Sync succeeded, want error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Sync error = %v, want %v", err, tt.wantErr)
			}

			// The previous workspace must survive every failure mode intact.
			assertTree(t, filepath.Join(s.root, "ws1"), map[string]treeEntry{
				"keep.txt":  {kind: 'f', data: "keep"},
				"sub":       {kind: 'd'},
				"sub/b.txt": {kind: 'f', data: "b"},
			})
		})
	}
}

func TestWorkspaceStore_SyncInvalidID(t *testing.T) {
	s, _ := newTestStore(t)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})
	if err := s.Sync(context.Background(), c, "ws id"); err == nil {
		t.Fatal("Sync with invalid id succeeded, want error")
	}
}
