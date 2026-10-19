package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
)

// fakeExecutor records every call and replays canned results, so tests can
// assert that tool side effects route through the injected Executor.
type fakeExecutor struct {
	execs   []executor.ExecRequest
	execRes executor.ExecResult
	execErr error
	reads   map[string][]byte
	mkdirs  []fakePath
	writes  []fakeWrite

	globCalls   []string
	globMatches map[string][]string
	statCalls   []string
	statInfo    map[string]fakeFileInfo
	grepCalls   []string
	grepMatches []executor.GrepMatch
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

type fakePath struct {
	path string
	perm fs.FileMode
}

type fakeWrite struct {
	path string
	data []byte
	perm fs.FileMode
}

func (f *fakeExecutor) Exec(_ context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	f.execs = append(f.execs, req)
	return f.execRes, f.execErr
}

func (f *fakeExecutor) Start(_ context.Context, _ executor.ExecRequest, _ executor.StartOptions) (executor.Process, error) {
	return nil, errors.New("fakeExecutor: Start not expected in this test")
}

func (f *fakeExecutor) ReadFile(_ context.Context, path string) ([]byte, error) {
	if data, ok := f.reads[path]; ok {
		return data, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeExecutor) WriteFile(_ context.Context, path string, data []byte, perm fs.FileMode) error {
	f.writes = append(f.writes, fakeWrite{path: path, data: data, perm: perm})
	return nil
}

func (f *fakeExecutor) MkdirAll(_ context.Context, path string, perm fs.FileMode) error {
	f.mkdirs = append(f.mkdirs, fakePath{path: path, perm: perm})
	return nil
}

func (f *fakeExecutor) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	f.statCalls = append(f.statCalls, path)
	if fi, ok := f.statInfo[path]; ok {
		return fi, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeExecutor) Glob(_ context.Context, pattern string) ([]string, error) {
	f.globCalls = append(f.globCalls, pattern)
	return f.globMatches[pattern], nil
}

func (f *fakeExecutor) Grep(_ context.Context, root string, pattern *regexp.Regexp) ([]executor.GrepMatch, error) {
	f.grepCalls = append(f.grepCalls, root)
	return f.grepMatches, nil
}

func TestBashToolRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{execRes: executor.ExecResult{Stdout: []byte("hello\n")}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Bash")

	res, err := tool.Invoke(t.Context(), map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.execs) != 1 {
		t.Fatalf("executor saw %d execs, want 1", len(fake.execs))
	}
	req := fake.execs[0]
	if req.Name != "bash" || len(req.Args) != 2 || req.Args[0] != "-c" || req.Args[1] != "echo hello" {
		t.Fatalf("request = %+v", req)
	}
	if req.Dir != dir {
		t.Fatalf("dir = %q, want %q", req.Dir, dir)
	}
	if res.Content != "hello\n" {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestBashToolExecutorResultMapping(t *testing.T) {
	dir := t.TempDir()

	// Non-zero exit code keeps the "Exit code N." wording and merges stderr.
	fake := &fakeExecutor{execRes: executor.ExecResult{Stdout: []byte("out\n"), Stderr: []byte("boom\n"), ExitCode: 7}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Bash")
	res, err := tool.Invoke(t.Context(), map[string]any{"command": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Exit code 7.\nout\n\nboom\n" {
		t.Fatalf("exit-code result = %q", res.Content)
	}

	// A startup failure maps to the non-ExitError branch ("Exit code -1.").
	fake = &fakeExecutor{execErr: errors.New("spawn failed")}
	reg = NewRegistryWithExecutor(dir, fake)
	tool, _ = reg.Get("Bash")
	res, err = tool.Invoke(t.Context(), map[string]any{"command": "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Content, "Exit code -1.") {
		t.Fatalf("start-failure result = %q", res.Content)
	}
}

func TestWriteToolRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeExecutor{reads: map[string][]byte{}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Write")

	res, err := tool.Invoke(t.Context(), map[string]any{
		"file_path": "sub/a.txt",
		"content":   "data",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != fmt.Sprintf("Wrote 4 bytes to %s", "sub/a.txt") {
		t.Fatalf("content = %q", res.Content)
	}

	wantDir := filepath.Join(dir, "sub")
	if len(fake.mkdirs) != 1 || fake.mkdirs[0].path != wantDir || fake.mkdirs[0].perm != 0o755 {
		t.Fatalf("mkdirs = %+v", fake.mkdirs)
	}
	wantPath := filepath.Join(dir, "sub", "a.txt")
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %+v", fake.writes)
	}
	w := fake.writes[0]
	if w.path != wantPath || string(w.data) != "data" || w.perm != 0o644 {
		t.Fatalf("write = %+v", w)
	}
}

func TestReadToolRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	wantPath := filepath.Join(dir, "f.txt")
	fake := &fakeExecutor{reads: map[string][]byte{wantPath: []byte("alpha\nbeta\n")}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Read")

	res, err := tool.Invoke(t.Context(), map[string]any{"file_path": "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "alpha") || !strings.Contains(res.Content, "beta") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestEditToolRoutesThroughExecutor(t *testing.T) {
	dir := t.TempDir()
	wantPath := filepath.Join(dir, "f.txt")
	fake := &fakeExecutor{reads: map[string][]byte{wantPath: []byte("hello world\n")}}
	reg := NewRegistryWithExecutor(dir, fake)
	tool, _ := reg.Get("Edit")

	res, err := tool.Invoke(t.Context(), map[string]any{
		"file_path":  "f.txt",
		"old_string": "world",
		"new_string": "there",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Edited f.txt (1 occurrence)" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(fake.writes) != 1 || fake.writes[0].path != wantPath || string(fake.writes[0].data) != "hello there\n" {
		t.Fatalf("writes = %+v", fake.writes)
	}
}
