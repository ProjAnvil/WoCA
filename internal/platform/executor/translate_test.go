package executor

import (
	"context"
	"io/fs"
	"regexp"
	"testing"
)

// recordingExec records the paths a Translate wrapper forwards to the inner
// executor and returns canned responses for the round-trip assertions.
type recordingExec struct {
	execReq  ExecRequest
	startReq ExecRequest
	reads    []string
	writes   []string
	mkdirs   []string
	stats    []string
	globs    []string
	greps    []string

	globMatches []string
	grepMatches []GrepMatch
}

func (r *recordingExec) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	r.execReq = req
	return ExecResult{}, nil
}

func (r *recordingExec) Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error) {
	r.startReq = req
	return nil, ErrSandboxNotImplemented
}

func (r *recordingExec) ReadFile(ctx context.Context, path string) ([]byte, error) {
	r.reads = append(r.reads, path)
	return nil, nil
}

func (r *recordingExec) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	r.writes = append(r.writes, path)
	return nil
}

func (r *recordingExec) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	r.mkdirs = append(r.mkdirs, path)
	return nil
}

func (r *recordingExec) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	r.stats = append(r.stats, path)
	return nil, fs.ErrNotExist
}

func (r *recordingExec) Glob(ctx context.Context, pattern string) ([]string, error) {
	r.globs = append(r.globs, pattern)
	return r.globMatches, nil
}

func (r *recordingExec) Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]GrepMatch, error) {
	r.greps = append(r.greps, root)
	return r.grepMatches, nil
}

// newTranslate builds a wrapper over a recording executor with the test's
// canonical roots.
func newTranslate() (*Translate, *recordingExec) {
	inner := &recordingExec{}
	return &Translate{Inner: inner, HostRoot: "/host/ws", SandboxRoot: "/workspace"}, inner
}

func TestTranslateRequestPathsToSandbox(t *testing.T) {
	ctx := context.Background()
	tx, inner := newTranslate()

	if _, err := tx.ReadFile(ctx, "/host/ws/a/b.txt"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := tx.WriteFile(ctx, "/host/ws/a/c.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tx.MkdirAll(ctx, "/host/ws/a/d", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := tx.Stat(ctx, "/host/ws/a"); err != nil && err != fs.ErrNotExist {
		t.Fatalf("Stat: %v", err)
	}
	if _, err := tx.Grep(ctx, "/host/ws/sub", regexp.MustCompile("x")); err != nil {
		t.Fatalf("Grep: %v", err)
	}

	if got, want := inner.reads[0], "/workspace/a/b.txt"; got != want {
		t.Errorf("ReadFile path: got %q, want %q", got, want)
	}
	if got, want := inner.writes[0], "/workspace/a/c.txt"; got != want {
		t.Errorf("WriteFile path: got %q, want %q", got, want)
	}
	if got, want := inner.mkdirs[0], "/workspace/a/d"; got != want {
		t.Errorf("MkdirAll path: got %q, want %q", got, want)
	}
	if got, want := inner.stats[0], "/workspace/a"; got != want {
		t.Errorf("Stat path: got %q, want %q", got, want)
	}
	if got, want := inner.greps[0], "/workspace/sub"; got != want {
		t.Errorf("Grep root: got %q, want %q", got, want)
	}
}

func TestTranslateExecDir(t *testing.T) {
	ctx := context.Background()
	tx, inner := newTranslate()

	req := ExecRequest{Name: "bash", Args: []string{"-c", "ls"}, Dir: "/host/ws/pkg"}
	if _, err := tx.Exec(ctx, req); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, want := inner.execReq.Dir, "/workspace/pkg"; got != want {
		t.Errorf("Exec Dir: got %q, want %q", got, want)
	}

	startReq := ExecRequest{Name: "bash", Args: []string{"-c", "watch"}, Dir: "/host/ws"}
	if _, err := tx.Start(ctx, startReq, StartOptions{}); err != nil {
		// inner returns ErrSandboxNotImplemented; only the recorded path matters.
		_ = err
	}
	if got, want := inner.startReq.Dir, "/workspace"; got != want {
		t.Errorf("Start Dir: got %q, want %q", got, want)
	}
}

func TestTranslateGlobRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx, inner := newTranslate()
	inner.globMatches = []string{"/workspace/x.go", "/workspace/sub/y.go"}

	matches, err := tx.Glob(ctx, "/host/ws/**/*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if got, want := inner.globs[0], "/workspace/**/*.go"; got != want {
		t.Errorf("Glob pattern: got %q, want %q", got, want)
	}
	want := []string{"/host/ws/x.go", "/host/ws/sub/y.go"}
	for i := range want {
		if matches[i] != want[i] {
			t.Errorf("match %d: got %q, want %q", i, matches[i], want[i])
		}
	}
}

func TestTranslateGrepMatchesBack(t *testing.T) {
	ctx := context.Background()
	tx, inner := newTranslate()
	inner.grepMatches = []GrepMatch{
		{Path: "/workspace/sub/y.go", Line: 3, Text: "x"},
	}

	matches, err := tx.Grep(ctx, "/host/ws/sub", regexp.MustCompile("x"))
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if got, want := matches[0].Path, "/host/ws/sub/y.go"; got != want {
		t.Errorf("GrepMatch.Path: got %q, want %q", got, want)
	}
	if matches[0].Line != 3 || matches[0].Text != "x" {
		t.Errorf("GrepMatch fields not preserved: %+v", matches[0])
	}
}

func TestTranslatePassthroughOutsideRoot(t *testing.T) {
	ctx := context.Background()
	tx, inner := newTranslate()

	if _, err := tx.ReadFile(ctx, "/etc/hosts"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := inner.reads[0]; got != "/etc/hosts" {
		t.Errorf("path outside HostRoot must pass through unchanged, got %q", got)
	}
}

func TestTranslateEmptyRootsIdentity(t *testing.T) {
	ctx := context.Background()
	inner := &recordingExec{}
	tx := &Translate{Inner: inner} // no roots configured: no mapping

	if _, err := tx.ReadFile(ctx, "/any/path"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := inner.reads[0]; got != "/any/path" {
		t.Errorf("empty roots must be identity, got %q", got)
	}
}

func TestTranslateImplementsExecutor(t *testing.T) {
	var _ Executor = (*Translate)(nil)
}
