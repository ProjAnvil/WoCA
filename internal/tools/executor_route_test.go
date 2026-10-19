package tools

// C-2 routing (design §10.1): search/notebook/image surfaces must reach
// the filesystem through the resolved executor, so a per-run sandbox
// serves them instead of the tool silently touching the host.

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
)

func fileDirFake(t *testing.T, dir string, files map[string]bool) *fakeExecutor {
	t.Helper()
	fake := &fakeExecutor{
		reads:       map[string][]byte{},
		globMatches: map[string][]string{},
		statInfo:    map[string]fakeFileInfo{},
	}
	fake.statInfo[dir] = fakeFileInfo{name: filepath.Base(dir), dir: true}
	// seed the recursive Glob+Stat enumeration: for every known directory
	// (the root plus every parent of a file) answer dir/* with its children.
	dirs := map[string][]string{dir: nil}
	for path := range files {
		child := path
		parent := filepath.Dir(child)
		for {
			dirs[parent] = append(dirs[parent], child)
			if parent == dir {
				break
			}
			child = parent
			parent = filepath.Dir(child)
			if parent == child {
				break
			}
		}
	}
	for parent, children := range dirs {
		fake.globMatches[filepath.Join(parent, "*")] = children
	}
	for path := range files {
		fake.statInfo[path] = fakeFileInfo{name: filepath.Base(path)}
	}
	for parent := range dirs {
		if parent != dir {
			fake.statInfo[parent] = fakeFileInfo{name: filepath.Base(parent), dir: true}
		}
	}
	return fake
}

func TestListDirRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{
		globMatches: map[string][]string{
			filepath.Join(dir, "*"): {filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub")},
		},
		statInfo: map[string]fakeFileInfo{
			dir:                        {name: filepath.Base(dir), dir: true},
			filepath.Join(dir, "a.txt"): {name: "a.txt"},
			filepath.Join(dir, "sub"):   {name: "sub", dir: true},
		},
	}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("ListDir")

	res, err := tool.Invoke(t.Context(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "a.txt\nsub/\n" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(fake.globCalls) == 0 {
		t.Fatal("ListDir did not reach the executor")
	}
}

func TestListDirMissingDirSurfacesError(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{statInfo: map[string]fakeFileInfo{}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("ListDir")

	res, err := tool.Invoke(t.Context(), map[string]any{"path": "missing-dir"})
	if err == nil || !strings.Contains(err.Error(), "missing-dir") {
		t.Fatalf("missing dir err = %v", err)
	}
	_ = res
}

func TestGlobRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	fake := fileDirFake(t, dir, map[string]bool{
		filepath.Join(dir, "a.go"):               true,
		filepath.Join(dir, "deep", "nest", "b.go"): true,
	})
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Glob")

	res, err := tool.Invoke(t.Context(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "deep/nest/b.go") {
		t.Fatalf("deep glob = %q", res.Content)
	}
	if len(fake.globCalls) == 0 || len(fake.statCalls) == 0 {
		t.Fatal("Glob did not reach the executor (Glob+Stat enumeration)")
	}
}

func TestGlobOnFileBaseRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "single.txt")
	fake := &fakeExecutor{statInfo: map[string]fakeFileInfo{
		file: {name: "single.txt"},
	}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Glob")

	res, err := tool.Invoke(t.Context(), map[string]any{"pattern": "single.txt", "path": "single.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "single.txt") {
		t.Fatalf("glob on file base = %q", res.Content)
	}
}

func TestGrepRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{grepMatches: []executor.GrepMatch{
		{Path: filepath.Join(dir, "sub", "y.go"), Line: 3, Text: "hello"},
	}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Grep")

	res, err := tool.Invoke(t.Context(), map[string]any{"pattern": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "sub/y.go:3:hello\n" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(fake.grepCalls) != 1 || fake.grepCalls[0] != dir {
		t.Fatalf("grep root = %+v", fake.grepCalls)
	}
}

func TestGrepIncludeFiltersExecutorMatches(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{grepMatches: []executor.GrepMatch{
		{Path: filepath.Join(dir, "a.md"), Line: 1, Text: "x"},
		{Path: filepath.Join(dir, "b.txt"), Line: 1, Text: "x"},
	}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Grep")

	res, err := tool.Invoke(t.Context(), map[string]any{"pattern": "x", "include": "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "a.md:1:x\n" {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestNotebookEditRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	nb := filepath.Join(dir, "nb.ipynb")
	fake := &fakeExecutor{reads: map[string][]byte{nb: []byte(`{"cells":[{"id":"c1","source":["old"],"type":"code"}]}`)}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("NotebookEdit")

	res, err := tool.Invoke(t.Context(), map[string]any{
		"notebook_path": "nb.ipynb", "cell_id": "c1", "new_source": "new line",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Edited cell 0") {
		t.Fatalf("content = %q", res.Content)
	}
	if len(fake.writes) != 1 || fake.writes[0].path != nb {
		t.Fatalf("writes = %+v", fake.writes)
	}
	if !strings.Contains(string(fake.writes[0].data), `"new line"`) {
		t.Fatalf("written notebook = %s", fake.writes[0].data)
	}
}

func TestReadImageRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(dir, "shot.png")
	fake := &fakeExecutor{reads: map[string][]byte{pngPath: buf.Bytes()}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Read")

	res, err := tool.Invoke(t.Context(), map[string]any{"file_path": "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata[MetaImageBase64] == nil {
		t.Fatalf("image result missing base64 metadata: %+v", res.Metadata)
	}
	if _, ok := fake.reads[pngPath]; !ok {
		t.Fatalf("image read did not reach the executor: %v", fake.reads)
	}
}

// Local-path regression: the Glob+Stat enumeration reproduces the previous
// WalkDir behavior (deep basename matching, per-directory walk).
func TestGlobLocalRegression(t *testing.T) {
	dir := t.TempDir()
	if err := mkdirAllWrite(filepath.Join(dir, "deep", "nest", "target.md"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllWrite(filepath.Join(dir, "top.go"), "x"); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "*.go"})
	if !strings.Contains(res.Content, "top.go") {
		t.Fatalf("local glob = %q", res.Content)
	}
	res = reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "**/*.md"})
	if !strings.Contains(res.Content, filepath.Join("deep", "nest", "target.md")) &&
		!strings.Contains(res.Content, "deep/nest/target.md") {
		t.Fatalf("local deep glob = %q", res.Content)
	}
	res = reg.Execute(t.Context(), "ListDir", map[string]any{})
	if !strings.Contains(res.Content, "deep/") || !strings.Contains(res.Content, "top.go") {
		t.Fatalf("local listdir = %q", res.Content)
	}
}

func mkdirAllWrite(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
