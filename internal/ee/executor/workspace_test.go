package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
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

// containerWorkspaceHostPath returns the host path backing a fake
// container's /workspace: fake-container commands see host paths, so
// in-container scripts must be written against this path.
func containerWorkspaceHostPath(t *testing.T, c Container) string {
	t.Helper()
	fc, ok := c.(*FakeContainer)
	if !ok {
		t.Fatalf("container is %T, want *FakeContainer", c)
	}
	return fc.hostPath(sandboxWorkspacePath)
}

// tarGz builds an in-memory tar.gz from the given headers (data written
// for regular entries in order).
func tarGz(t *testing.T, entries []tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, hdr := range entries {
		h := hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader(%q): %v", hdr.Name, err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			data := strings.Repeat("x", int(h.Size))
			if _, err := tw.Write([]byte(data)); err != nil {
				t.Fatalf("Write(%q): %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// importArchive feeds body through the store's GuardImport into the
// container's /workspace (the manual import path) and returns the CopyIn
// error.
func importArchive(t *testing.T, s *WorkspaceStore, c Container, body []byte) error {
	t.Helper()
	return c.CopyIn(context.Background(), sandboxWorkspacePath, s.GuardImport(bytes.NewReader(body)))
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

// --- GuardImport (manual archive channel checks) ---------------------------

// A clean manual import lands in the container workspace: files,
// directories and in-root symlinks survive, .git included (it is the
// user's own archive, spec §5.3).
func TestWorkspaceStore_GuardImportRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	body := tarGz(t, []tar.Header{
		{Name: "main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		{Name: "sub", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "sub/util.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		{Name: ".git/config", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "main.go"},
	})
	if err := importArchive(t, s, c, body); err != nil {
		t.Fatalf("import: %v", err)
	}

	wsHost := containerWorkspaceHostPath(t, c)
	for name, want := range map[string]string{
		"main.go":      "xxx",
		"sub/util.txt": "xxxxx",
		".git/config":  "xxxxxx",
	} {
		data, err := os.ReadFile(filepath.Join(wsHost, name))
		if err != nil {
			t.Fatalf("imported %s missing: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("imported %s = %q, want %q", name, data, want)
		}
	}
	link, err := os.Readlink(filepath.Join(wsHost, "link"))
	if err != nil || link != "main.go" {
		t.Errorf("imported link = %q err=%v, want main.go", link, err)
	}
}

// The channel checks (spec §5.3): absolute entry names, ".." traversal,
// and escaping or absolute symlink targets are rejected mid-stream — the
// CopyIn error carries them and nothing is written past the violation.
func TestWorkspaceStore_GuardImportRejectsEscapes(t *testing.T) {
	tests := []struct {
		name    string
		entries []tar.Header
		wantErr bool
	}{
		{
			name: "absolute entry",
			entries: []tar.Header{
				{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
				{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
			},
			wantErr: true,
		},
		{
			name: "dotdot traversal",
			entries: []tar.Header{
				{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
			},
			wantErr: true,
		},
		{
			name: "deep dotdot traversal",
			entries: []tar.Header{
				{Name: "sub/../../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
			},
			wantErr: true,
		},
		{
			name: "absolute symlink",
			entries: []tar.Header{
				{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
			},
			wantErr: true,
		},
		{
			name: "escaping symlink",
			entries: []tar.Header{
				{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../outside"},
			},
			wantErr: true,
		},
		{
			name: "nested escaping symlink",
			entries: []tar.Header{
				{Name: "sub/evil", Typeflag: tar.TypeSymlink, Linkname: "../../outside"},
			},
			wantErr: true,
		},
		{
			// A link one level up from a subdirectory resolves to the
			// workspace root — in-root, allowed (the one-hop policy).
			name: "in-root parent link allowed",
			entries: []tar.Header{
				{Name: "sub/ok", Typeflag: tar.TypeSymlink, Linkname: "../file"},
				{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			be := newBackend(t)
			c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

			err := importArchive(t, s, c, tarGz(t, tt.entries))
			if tt.wantErr && err == nil {
				t.Fatal("import succeeded, want channel-check error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("import: %v", err)
			}
		})
	}
}

// An import past the archive cap is rejected with ErrWorkspaceTooLarge
// (settings key sandbox-archive-max-bytes); the cap counts the
// uncompressed tar payload, so gzip cannot slip a big workspace past it.
func TestWorkspaceStore_GuardImportSizeCap(t *testing.T) {
	s := NewWorkspaceStore(t.TempDir(), 1024)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	// Highly compressible payload: 64 KiB of zeros, well over the 1 KiB
	// cap once uncompressed.
	body := tarGz(t, []tar.Header{
		{Name: "big.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: 64 << 10},
	})
	err := importArchive(t, s, c, body)
	if !errors.Is(err, ErrWorkspaceTooLarge) {
		t.Fatalf("import over cap: got %v, want ErrWorkspaceTooLarge", err)
	}
}

// Non-regular entries (hardlinks, fifos, devices) are dropped, not
// unpacked — the sanitized stream only carries files, dirs and safe
// symlinks.
func TestWorkspaceStore_GuardImportDropsSpecialEntries(t *testing.T) {
	s, _ := newTestStore(t)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	entries := []tar.Header{
		{Name: "fifo", Typeflag: tar.TypeFifo},
		{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o644, Size: 0},
		{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
	}
	if err := importArchive(t, s, c, tarGz(t, entries)); err != nil {
		t.Fatalf("import: %v", err)
	}
	wsHost := containerWorkspaceHostPath(t, c)
	if _, err := os.Lstat(filepath.Join(wsHost, "ok.txt")); err != nil {
		t.Fatalf("ok.txt missing: %v", err)
	}
	for _, name := range []string{"fifo", "dev"} {
		if _, err := os.Lstat(filepath.Join(wsHost, name)); !os.IsNotExist(err) {
			t.Errorf("%s unpacked, want dropped", name)
		}
	}
}

// GuardImport passes plain (uncompressed) tars through the same checks —
// CopyIn accepts both shapes.
func TestWorkspaceStore_GuardImportPlainTar(t *testing.T) {
	s, _ := newTestStore(t)
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "test-image", WorkDir: "/workspace"})

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "/abs", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := importArchive(t, s, c, buf.Bytes()); err == nil {
		t.Fatal("plain-tar import with absolute entry succeeded, want error")
	}
}
