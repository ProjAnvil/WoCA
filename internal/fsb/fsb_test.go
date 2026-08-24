package fsb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListDirectoryOnly(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "alpha", "inner"), filepath.Join(base, "beta"))
	if err := os.WriteFile(filepath.Join(base, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New([]string{base}, false)
	out, err := s.List(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Dirs) != 2 || out.Dirs[0].Name != "alpha" || out.Dirs[1].Name != "beta" {
		t.Fatalf("dirs = %+v", out.Dirs)
	}
	if out.Dirs[0].Path != resolveExisting(filepath.Join(base, "alpha")) || !out.Dirs[0].Writable {
		t.Fatalf("entry = %+v", out.Dirs[0])
	}
	if out.Parent != "" {
		t.Fatalf("parent at a jail root = %q, want empty", out.Parent)
	}

	inner, err := s.List(filepath.Join(base, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if inner.Path != resolveExisting(filepath.Join(base, "alpha")) {
		t.Fatalf("inner path = %q, want resolved", inner.Path)
	}
	if inner.Parent != resolveExisting(base) {
		t.Fatalf("inner parent = %q", inner.Parent)
	}
	if len(inner.Dirs) != 1 || inner.Dirs[0].Name != "inner" {
		t.Fatalf("inner dirs = %+v", inner.Dirs)
	}
}

func TestListEmptyPathShowsRoots(t *testing.T) {
	base := t.TempDir()
	s := New([]string{base}, false)
	out, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Roots) != 1 || out.Roots[0] != resolveExisting(base) {
		t.Fatalf("roots = %+v", out.Roots)
	}
	if len(out.Dirs) != 1 || out.Dirs[0].Path != resolveExisting(base) {
		t.Fatalf("dirs = %+v", out.Dirs)
	}
}

func TestJailBlocksTraversalAndEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, filepath.Join(base, "ok"), filepath.Join(outside, "secret"))
	s := New([]string{base}, false)

	// Traversal that stays inside the jail cleans back to the root and
	// succeeds; traversal that EXITS the jail is rejected.
	if _, err := s.List(filepath.Join(base, "ok", "..")); err != nil {
		t.Fatalf("in-jail traversal err = %v", err)
	}
	if _, err := s.List(base + string(filepath.Separator) + ".."); !errors.Is(err, ErrForbidden) {
		t.Fatalf("exiting traversal err = %v", err)
	}
	// Outside the jail entirely.
	if _, err := s.List(outside); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outside err = %v", err)
	}
	// Prefix-similar path outside the jail (classic string-check bug).
	similar := base + "-suffix"
	if _, err := s.List(similar); !errors.Is(err, ErrForbidden) {
		t.Fatalf("prefix-similar err = %v", err)
	}
	// Symlink pointing out of the jail: listed dir vanishes (stat through
	// the root fails), and listing THROUGH the symlink is forbidden.
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	out, err := s.List(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Dirs {
		if d.Name == "escape" {
			t.Fatal("symlinked dir leaked into listing")
		}
	}
	if _, err := s.List(filepath.Join(base, "escape")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("through-symlink err = %v", err)
	}
}

func TestMkdirSemantics(t *testing.T) {
	base := t.TempDir()
	s := New([]string{base}, false)

	p, err := s.Mkdir(base, "fresh")
	if err != nil || p != filepath.Join(base, "fresh") {
		t.Fatalf("mkdir = %q %v", p, err)
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		t.Fatalf("created = %v %v", st, err)
	}
	if _, err := s.Mkdir(base, "fresh"); !errors.Is(err, ErrExists) {
		t.Fatalf("exists err = %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, strings200} {
		if _, err := s.Mkdir(base, bad); !errors.Is(err, ErrBadName) {
			t.Fatalf("bad name %q err = %v", bad, err)
		}
	}
	// Parent outside the jail.
	if _, err := s.Mkdir(t.TempDir(), "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outside parent err = %v", err)
	}
	// Parent is a file.
	f := filepath.Join(base, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mkdir(f, "x"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file parent err = %v", err)
	}
}

const strings200 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
	"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestValidate(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "d"))
	s := New([]string{base}, false)

	if e, d, w := s.Validate(filepath.Join(base, "d")); !e || !d || !w {
		t.Fatalf("dir = %v %v %v", e, d, w)
	}
	if e, _, _ := s.Validate(filepath.Join(base, "missing")); e {
		t.Fatal("missing reported as existing")
	}
	if e, _, _ := s.Validate(t.TempDir()); e {
		t.Fatal("outside jail reported as existing")
	}
}

func TestWatchEmitsOnChange(t *testing.T) {
	base := t.TempDir()
	s := New([]string{base}, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := s.Watch(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the poller take a baseline
	if err := os.Mkdir(filepath.Join(base, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no change event")
	}
	cancel()
}

func TestWatchRejectsEmptyAndForbidden(t *testing.T) {
	base := t.TempDir()
	s := New([]string{base}, false)
	if _, err := s.Watch(t.Context(), ""); !errors.Is(err, ErrNotDir) {
		t.Fatalf("empty err = %v", err)
	}
	if _, err := s.Watch(t.Context(), t.TempDir()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outside err = %v", err)
	}
}

func TestPermissiveJailsToFilesystemRoot(t *testing.T) {
	s := New(nil, true)
	if _, err := s.List(t.TempDir()); err != nil {
		t.Fatalf("permissive list err = %v", err)
	}
	if _, err := s.List(string(filepath.Separator)); err != nil {
		t.Fatalf("root list err = %v", err)
	}
}

func TestDefaultRootsFallBackToHome(t *testing.T) {
	s := New(nil, false)
	roots := s.Roots()
	if len(roots) == 0 {
		t.Fatal("no default root")
	}
}
