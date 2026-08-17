package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildWorkspaceTreeEmptyRoot(t *testing.T) {
	if tree := buildWorkspaceTree(""); tree != nil {
		t.Fatalf("empty root tree = %+v, want nil", tree)
	}
}

func TestWalkWorkspaceMissing(t *testing.T) {
	dir := t.TempDir()
	if node := walkWorkspace(dir, filepath.Join(dir, "missing"), 0); node != nil {
		t.Fatalf("missing dir node = %+v, want nil", node)
	}
}

func TestWalkWorkspaceDepthCap(t *testing.T) {
	root := t.TempDir()
	// Chain one dir deeper than workspaceMaxDepth.
	deep := root
	for i := 0; i < workspaceMaxDepth+3; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(filepath.Join(deep, "beyond"), 0o755); err != nil {
		t.Fatal(err)
	}

	node := walkWorkspace(root, root, 0)
	depth := 0
	for len(node.Children) > 0 {
		node = node.Children[0]
		depth++
	}
	if depth != workspaceMaxDepth {
		t.Fatalf("walked %d levels, want the %d-level cap", depth, workspaceMaxDepth)
	}
}

func TestWalkWorkspaceEntryCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < workspaceMaxEntries+50; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%04d", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	node := walkWorkspace(dir, dir, 0)
	if len(node.Children) != workspaceMaxEntries {
		t.Fatalf("children = %d, want the %d cap", len(node.Children), workspaceMaxEntries)
	}
}

func TestWalkWorkspaceUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	// The dir itself still yields a (childless) node instead of failing.
	node := walkWorkspace(dir, locked, 0)
	if node == nil || node.Kind != "dir" || len(node.Children) != 0 {
		t.Fatalf("unreadable dir node = %+v", node)
	}
}

func TestNodePathRelError(t *testing.T) {
	// Mixed absolute/relative inputs cannot be made relative: the base name
	// is the fallback.
	if got := nodePath("rel-root", "/abs/child.txt"); got != "child.txt" {
		t.Fatalf("nodePath rel fallback = %q, want child.txt", got)
	}
	if got := nodePath("/root", "/root"); got != "root" {
		t.Fatalf("nodePath(root) = %q, want root", got)
	}
	if got := nodePath("/root", "/root/a/b.txt"); got != "a/b.txt" {
		t.Fatalf("nodePath nested = %q, want a/b.txt", got)
	}
}

func TestIsPreviewable(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".gitignore", true},
		{".env", true},
		{".editorconfig", true},
		{".npmrc", true},
		{".nvmrc", true},
		{".gitattributes", true},
		{".unknownrc", false},
		{".config", false}, // dotfile without a known name
		{"main.go", true},
		{"app.css", true},
		{"Dockerfile", false}, // no extension
		{"photo.png", false},
		{"archive.zip", false},
	}
	for _, c := range cases {
		if got := isPreviewable(c.name); got != c.want {
			t.Errorf("isPreviewable(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadWorkspaceFileErrors(t *testing.T) {
	root := t.TempDir()
	// Empty root / blank rel path.
	if _, err := readWorkspaceFile("", "x.go"); !errors.Is(err, errNotFound) {
		t.Fatalf("empty root err = %v", err)
	}
	if _, err := readWorkspaceFile(root, "   "); !errors.Is(err, errNotFound) {
		t.Fatalf("blank rel err = %v", err)
	}

	// Oversized previewable file.
	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, make([]byte, workspaceMaxFilePreview+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceFile(root, "big.txt"); !errors.Is(err, errFileTooLarge) {
		t.Fatalf("oversized err = %v, want errFileTooLarge", err)
	}

	// Invalid UTF-8 content in a previewable type.
	bin := filepath.Join(root, "bin.txt")
	if err := os.WriteFile(bin, []byte{0xff, 0xfe, 'h', 'i'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceFile(root, "bin.txt"); !errors.Is(err, errNotPreviewable) {
		t.Fatalf("binary err = %v, want errNotPreviewable", err)
	}

	// A directory is not a previewable file.
	if _, err := readWorkspaceFile(root, "."); !errors.Is(err, errNotFound) {
		t.Fatalf("dir err = %v, want errNotFound", err)
	}
}

func TestReadWorkspaceFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked.txt")
	if err := os.WriteFile(locked, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	// Stat succeeds, the read does not: mapped to 404.
	if _, err := readWorkspaceFile(root, "locked.txt"); !errors.Is(err, errNotFound) {
		t.Fatalf("unreadable err = %v, want errNotFound", err)
	}
}
