package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()

	tests := []struct {
		name       string
		req        ExecRequest
		wantOut    string
		wantStderr string
		wantCode   int
		wantErr    bool
		wantErrIs  error
	}{
		{
			name:     "success captures stdout",
			req:      ExecRequest{Name: "echo", Args: []string{"hello"}},
			wantOut:  "hello\n",
			wantCode: 0,
		},
		{
			name:       "non-zero exit is not an error",
			req:        ExecRequest{Name: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}},
			wantOut:    "out\n",
			wantStderr: "err\n",
			wantCode:   3,
		},
		{
			name:     "stdin is piped",
			req:      ExecRequest{Name: "cat", Stdin: []byte("from-stdin")},
			wantOut:  "from-stdin",
			wantCode: 0,
		},
		{
			name:     "dir is honored",
			req:      ExecRequest{Name: "pwd", Dir: tmp},
			wantCode: 0,
		},
		{
			name:     "env is honored",
			req:      ExecRequest{Name: "sh", Args: []string{"-c", "printf %s \"$WOCA_TEST_VAR\""}, Env: []string{"WOCA_TEST_VAR=ok"}},
			wantOut:  "ok",
			wantCode: 0,
		},
		{
			name:      "missing command returns error",
			req:       ExecRequest{Name: "woca-no-such-command-xyz"},
			wantErr:   true,
			wantErrIs: exec.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := e.Exec(ctx, tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (res=%+v)", res)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("error %v should wrap %v", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ExitCode != tt.wantCode {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, tt.wantCode)
			}
			if tt.wantOut != "" {
				got := string(res.Stdout)
				if tt.name == "dir is honored" {
					if strings.TrimSpace(got) != tmp {
						t.Errorf("Stdout = %q, want dir %q", got, tmp)
					}
				} else if got != tt.wantOut {
					t.Errorf("Stdout = %q, want %q", got, tt.wantOut)
				}
			}
			if tt.wantStderr != "" && string(res.Stderr) != tt.wantStderr {
				t.Errorf("Stderr = %q, want %q", res.Stderr, tt.wantStderr)
			}
		})
	}
}

func TestExecContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := NewLocal().Exec(ctx, ExecRequest{Name: "sleep", Args: []string{"5"}})
	if err == nil {
		t.Fatalf("expected error on context cancel, got nil (res=%+v)", res)
	}
	if ctx.Err() == nil {
		t.Fatal("context should be done")
	}
}

func TestReadFile(t *testing.T) {
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()
	p := writeTestFile(t, tmp, "a.txt", "content-a")

	data, err := e.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "content-a" {
		t.Errorf("ReadFile = %q, want %q", data, "content-a")
	}

	_, err = e.ReadFile(ctx, filepath.Join(tmp, "missing.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile missing: err = %v, want fs.ErrNotExist", err)
	}
}

func TestWriteFileAndReadBack(t *testing.T) {
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()
	p := filepath.Join(tmp, "sub", "w.txt")

	// Parent dir does not exist yet: create it first, like callers do.
	if err := e.MkdirAll(ctx, filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := e.WriteFile(ctx, p, []byte("data-1"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := e.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "data-1" {
		t.Errorf("ReadFile = %q, want %q", data, "data-1")
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", info.Mode().Perm())
	}
}

func TestMkdirAllAndStat(t *testing.T) {
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "x", "y", "z")

	if err := e.MkdirAll(ctx, dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := e.Stat(ctx, dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("Stat should report a directory")
	}

	// MkdirAll on an existing directory is a no-op.
	if err := e.MkdirAll(ctx, dir, 0o755); err != nil {
		t.Fatalf("MkdirAll existing: %v", err)
	}

	_, err = e.Stat(ctx, filepath.Join(tmp, "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat missing: err = %v, want fs.ErrNotExist", err)
	}

	// Stat on a file.
	fp := writeTestFile(t, tmp, "f.txt", "x")
	fi, err := e.Stat(ctx, fp)
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if fi.IsDir() || fi.Name() != "f.txt" {
		t.Errorf("Stat file: %+v", fi)
	}
}

func TestGlob(t *testing.T) {
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()
	writeTestFile(t, tmp, "a.go", "")
	writeTestFile(t, tmp, "b.go", "")
	writeTestFile(t, tmp, "c.txt", "")
	writeTestFile(t, tmp, "sub/d.go", "")

	matches, err := e.Glob(ctx, filepath.Join(tmp, "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	sort.Strings(matches)
	if len(matches) != 2 || filepath.Base(matches[0]) != "a.go" || filepath.Base(matches[1]) != "b.go" {
		t.Errorf("Glob = %v, want a.go b.go", matches)
	}

	// No match returns empty, not an error.
	matches, err = e.Glob(ctx, filepath.Join(tmp, "*.rs"))
	if err != nil {
		t.Fatalf("Glob no match: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("Glob = %v, want empty", matches)
	}

	// Bad pattern returns an error.
	if _, err := e.Glob(ctx, "["); err == nil {
		t.Error("Glob bad pattern: expected error")
	}
}

func TestGrep(t *testing.T) {
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()
	writeTestFile(t, tmp, "one.txt", "alpha\nneedle one\nomega\n")
	writeTestFile(t, tmp, "sub/two.txt", "nothing\nneedle two\n")
	writeTestFile(t, tmp, ".hidden/three.txt", "needle hidden\n")
	writeTestFile(t, tmp, "sub/.config/four.txt", "needle nested hidden\n")

	re := regexp.MustCompile(`needle (\w+)`)
	matches, err := e.Grep(ctx, tmp, re)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	// Matches from hidden directories are skipped.
	type key struct {
		base string
		line int
	}
	var got []key
	for _, m := range matches {
		got = append(got, key{filepath.Base(m.Path), m.Line})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].base != got[j].base {
			return got[i].base < got[j].base
		}
		return got[i].line < got[j].line
	})
	want := []key{{"one.txt", 2}, {"two.txt", 2}}
	if len(got) != len(want) {
		t.Fatalf("Grep matches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("match[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// Match text is the full line.
	for _, m := range matches {
		if !strings.Contains(m.Text, "needle") {
			t.Errorf("match text %q should contain needle", m.Text)
		}
	}

	// Root itself may be a dot directory; it must not be skipped.
	dotRoot := filepath.Join(tmp, ".root")
	writeTestFile(t, dotRoot, "in.txt", "needle dotroot\n")
	matches, err = e.Grep(ctx, dotRoot, re)
	if err != nil {
		t.Fatalf("Grep dot root: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("Grep dot root = %v, want 1 match", matches)
	}

	// Unreadable/missing root returns an error.
	if _, err := e.Grep(ctx, filepath.Join(tmp, "nope"), re); err == nil {
		t.Error("Grep missing root: expected error")
	}
}

func TestGrepSkipsUnreadableAndLongLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are unreliable on windows")
	}
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()

	// Long lines beyond the bufio default (64 KiB) must still be scanned.
	longLine := strings.Repeat("x", 256*1024) + " needle-long"
	writeTestFile(t, tmp, "long.txt", longLine+"\n")
	writeTestFile(t, tmp, "ok.txt", "needle-short\n")

	re := regexp.MustCompile("needle")
	matches, err := e.Grep(ctx, tmp, re)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("Grep matches = %d, want 2 (long line should not abort)", len(matches))
	}
}

func TestGrepSkipsUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are unreliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	ctx := context.Background()
	e := NewLocal()
	tmp := t.TempDir()

	writeTestFile(t, tmp, "ok.txt", "needle ok\n")
	blocked := writeTestFile(t, tmp, "blocked.txt", "needle blocked\n")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o644) })
	blockedDir := filepath.Join(tmp, "blockeddir")
	writeTestFile(t, blockedDir, "in.txt", "needle dir\n")
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o755) })

	matches, err := e.Grep(ctx, tmp, regexp.MustCompile("needle"))
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0].Path) != "ok.txt" {
		t.Errorf("Grep matches = %v, want only ok.txt", matches)
	}
}

func TestContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := NewLocal()
	tmp := t.TempDir()

	tests := []struct {
		name string
		call func() error
	}{
		{"ReadFile", func() error { _, err := e.ReadFile(ctx, tmp); return err }},
		{"WriteFile", func() error { return e.WriteFile(ctx, filepath.Join(tmp, "x"), nil, 0o644) }},
		{"MkdirAll", func() error { return e.MkdirAll(ctx, filepath.Join(tmp, "d"), 0o755) }},
		{"Stat", func() error { _, err := e.Stat(ctx, tmp); return err }},
		{"Glob", func() error { _, err := e.Glob(ctx, "*"); return err }},
		{"Grep", func() error { _, err := e.Grep(ctx, tmp, regexp.MustCompile("x")); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		})
	}
}

func TestNewLocalImplementsExecutor(t *testing.T) {
	var _ Executor = NewLocal()
}

func TestStartRoundTripsStdio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	p, err := NewLocal().Start(context.Background(),
		ExecRequest{Name: "sh", Args: []string{"-c", "read line; echo \"got:$line\"; echo err >&2"}},
		StartOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stdin().Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "got:hello" {
		t.Fatalf("stdout round trip: %q", out)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestStartRedirectsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	var buf bytes.Buffer
	p, err := NewLocal().Start(context.Background(),
		ExecRequest{Name: "sh", Args: []string{"-c", "echo one; echo two >&2; exit 3"}},
		StartOptions{Stdout: &buf, Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if p.Stdout() != nil {
		t.Fatal("redirected stdout must not expose a pipe")
	}
	waitErr := p.Wait()
	var ee *ExitError
	if !errors.As(waitErr, &ee) || ee.Code != 3 {
		t.Fatalf("wait error = %v, want exit status 3", waitErr)
	}
	if ee.Error() != "exit status 3" {
		t.Fatalf("error text %q must match os/exec wording", ee.Error())
	}
	if got := strings.TrimSpace(buf.String()); got != "one\ntwo" {
		t.Fatalf("redirected output: %q", got)
	}
}

func TestStartKillTerminatesAndWaitReaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	p, err := NewLocal().Start(context.Background(),
		ExecRequest{Name: "sh", Args: []string{"-c", "sleep 30"}},
		StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := p.Wait()
	if waitErr == nil {
		t.Fatal("killed process must report a non-nil wait error")
	}
	var ee *ExitError
	if errors.As(waitErr, &ee) && ee.Code == 0 {
		t.Fatal("killed process must not report exit code 0")
	}
	// A second kill of the reaped process reports an error callers may ignore.
	if err := p.Kill(); err == nil {
		t.Log("second kill unexpectedly succeeded")
	}
}

func TestStartStartupFailure(t *testing.T) {
	if _, err := NewLocal().Start(context.Background(),
		ExecRequest{Name: "definitely-not-a-real-binary-xyz"}, StartOptions{}); err == nil {
		t.Fatal("missing binary must fail at Start")
	}
}

func TestStartContextCancelKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX shell commands")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := NewLocal().Start(ctx, ExecRequest{Name: "sh", Args: []string{"-c", "sleep 30"}}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitErr := p.Wait()
	if waitErr == nil {
		t.Fatal("canceling the start context must kill the process")
	}
}
