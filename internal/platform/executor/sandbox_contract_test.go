// sandbox_contract_test.go is the M1 close-out compliance test: it drives
// the Sandbox Executor (contract v2 client) against the fakesandbox
// backend over real HTTP — every Executor method, the Process lifecycle,
// and the archive round-trip with permission bits preserved.
package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
)

// newContractPair returns a Sandbox executor backed by a fakesandbox
// server, both bound to the test lifetime.
func newContractPair(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := NewSandbox(SandboxConfig{Endpoint: fakesandbox.Addr(t)})
	if err != nil {
		t.Fatalf("NewSandbox against fakesandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func TestContractExec(t *testing.T) {
	sb := newContractPair(t)

	// exit code 0, stdout round-trip
	res, err := sb.Exec(t.Context(), ExecRequest{Name: "echo", Args: []string{"contract"}, Dir: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "contract\n" {
		t.Fatalf("res = %+v stdout=%q", res, res.Stdout)
	}

	// env override reaches the process
	res, err = sb.Exec(t.Context(), ExecRequest{
		Name: "sh", Args: []string{"-c", "printf %s $WOCATEST"},
		Env: []string{"WOCATEST=env-ok"},
	})
	if err != nil || string(res.Stdout) != "env-ok" {
		t.Fatalf("env exec: err=%v stdout=%q", err, res.Stdout)
	}

	// stdin piping
	res, err = sb.Exec(t.Context(), ExecRequest{Name: "cat", Stdin: []byte("piped-stdin\n")})
	if err != nil || string(res.Stdout) != "piped-stdin\n" {
		t.Fatalf("stdin exec: err=%v stdout=%q", err, res.Stdout)
	}

	// non-zero exit code is a result, not an error
	res, err = sb.Exec(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 7"}})
	if err != nil {
		t.Fatalf("non-zero exit returned error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}
	if string(res.Stderr) != "boom\n" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestContractReadWriteFile(t *testing.T) {
	sb := newContractPair(t)

	if err := sb.WriteFile(t.Context(), "/workspace/notes.txt", []byte("round trip"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := sb.ReadFile(t.Context(), "/workspace/notes.txt")
	if err != nil || string(data) != "round trip" {
		t.Fatalf("read = %q err=%v", data, err)
	}

	// binary payload survives the base64 wire
	bin := make([]byte, 256)
	for i := range bin {
		bin[i] = byte(i)
	}
	if err := sb.WriteFile(t.Context(), "/workspace/bin.dat", bin, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sb.ReadFile(t.Context(), "/workspace/bin.dat")
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("binary round-trip mismatch err=%v", err)
	}

	if _, err := sb.ReadFile(t.Context(), "/workspace/nope.txt"); err == nil {
		t.Fatal("read of missing file succeeded")
	}
}

func TestContractStatMkdirGlob(t *testing.T) {
	sb := newContractPair(t)

	if err := sb.MkdirAll(t.Context(), "/workspace/pkg/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(t.Context(), "/workspace/pkg/a.go", []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(t.Context(), "/workspace/pkg/b.go", []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := sb.Stat(t.Context(), "/workspace/pkg/sub")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatal("MkdirAll'd path not reported as dir")
	}

	fi, err = sb.Stat(t.Context(), "/workspace/pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() || fi.Name() != "a.go" || fi.Size() != int64(len("package pkg\n")) || fi.Mode().Perm() != 0o644 {
		t.Fatalf("stat = name:%q size:%d mode:%o", fi.Name(), fi.Size(), fi.Mode().Perm())
	}

	matches, err := sb.Glob(t.Context(), "/workspace/pkg/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("glob matches = %v", matches)
	}

	if _, err := sb.Stat(t.Context(), "/workspace/missing"); err == nil {
		t.Fatal("stat of missing path succeeded")
	}
}

func TestContractGrepSkipsHidden(t *testing.T) {
	sb := newContractPair(t)

	write := func(p, text string) {
		t.Helper()
		if err := sb.WriteFile(t.Context(), p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("/workspace/open.txt", "needle here\n")
	write("/workspace/.git/config", "needle in git\n")
	write("/workspace/.hidden/h.txt", "needle hidden\n")
	write("/workspace/sub/c.txt", "needle deep\n")

	matches, err := sb.Grep(t.Context(), "/workspace", regexp.MustCompile(`needle`))
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, m.Path)
		if m.Line != 1 || m.Text != "needle here" && m.Text != "needle deep" {
			t.Fatalf("match = %+v", m)
		}
	}
	want := []string{"/workspace/open.txt", "/workspace/sub/c.txt"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("grep paths = %v, want %v (hidden dirs skipped)", paths, want)
	}
}

func TestContractProcessLifecycle(t *testing.T) {
	sb := newContractPair(t)

	// stdin/stdout pipe round-trip + EOF + Wait nil
	p, err := sb.Start(t.Context(), ExecRequest{Name: "cat"}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stdin().Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stdin().Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := p.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello world" {
		t.Fatalf("stdout = %q", out)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// non-zero exit surfaces from Wait as *ExitError
	p, err = sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "exit 5"}}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Wait()
	if exitErr, ok := errors.AsType[*ExitError](err); !ok || exitErr.Code != 5 {
		t.Fatalf("Wait err = %v, want *ExitError{5}", err)
	}

	// Kill unblocks Wait; redirected stdout + stderr sinks work
	var outBuf, errBuf bytes.Buffer
	p, err = sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "echo to-out; echo to-err >&2; sleep 60"}}, StartOptions{Stdout: &outBuf, Stderr: &errBuf})
	if err != nil {
		t.Fatal(err)
	}
	if p.Stdout() != nil {
		t.Fatal("Stdout() must be nil when redirected")
	}
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for outBuf.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
	if outBuf.String() != "to-out\n" {
		t.Fatalf("redirected stdout = %q", outBuf.String())
	}
	if errBuf.String() != "to-err\n" {
		t.Fatalf("stderr sink = %q", errBuf.String())
	}

	// ctx cancel kills the process (Executor.Start contract)
	ctx, cancel := context.WithCancel(t.Context())
	p, err = sb.Start(ctx, ExecRequest{Name: "sh", Args: []string{"-c", "sleep 60"}}, StartOptions{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	waited2 := make(chan error, 1)
	go func() { waited2 <- p.Wait() }()
	select {
	case <-waited2:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

func TestContractArchiveRoundTrip(t *testing.T) {
	sb := newContractPair(t)

	// Upload: script (0755), private file (0600), dir, symlink.
	var upload bytes.Buffer
	zw := gzip.NewWriter(&upload)
	tw := tar.NewWriter(zw)
	mustHeader := func(name string, mode int64, typ byte, size int64, link string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Typeflag: typ, Size: size, Linkname: link, ModTime: time.UnixMilli(0)}); err != nil {
			t.Fatal(err)
		}
	}
	mustHeader("run.sh", 0o755, tar.TypeReg, int64(len("#!/bin/sh\n")), "")
	if _, err := tw.Write([]byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	mustHeader("secret.key", 0o600, tar.TypeReg, int64(len("k")), "")
	if _, err := tw.Write([]byte("k")); err != nil {
		t.Fatal(err)
	}
	mustHeader("pkg", 0o755, tar.TypeDir, 0, "")
	mustHeader("pkg/data.bin", 0o640, tar.TypeReg, 3, "")
	if _, err := tw.Write([]byte{0xCA, 0xFE, 0x00}); err != nil {
		t.Fatal(err)
	}
	mustHeader("latest", 0o777, tar.TypeSymlink, 0, "run.sh")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := sb.PutArchive(t.Context(), bytes.NewReader(upload.Bytes())); err != nil {
		t.Fatalf("PutArchive: %v", err)
	}

	// Files landed under /workspace with modes intact.
	data, err := sb.ReadFile(t.Context(), "/workspace/run.sh")
	if err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("run.sh after upload = %q err=%v", data, err)
	}
	fi, err := sb.Stat(t.Context(), "/workspace/secret.key")
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret.key stat: err=%v mode=%o", err, fi.Mode().Perm())
	}

	// Download: entries relative to workspace root, modes + symlink kept.
	rc, err := sb.GetArchive(t.Context())
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	hdrs := map[string]*tar.Header{}
	contents := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		hdrs[hdr.Name] = hdr
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			contents[hdr.Name] = string(b)
		}
	}
	if h := hdrs["run.sh"]; h == nil || h.Mode != 0o755 || contents["run.sh"] != "#!/bin/sh\n" {
		t.Fatalf("run.sh header = %+v content=%q", hdrs["run.sh"], contents["run.sh"])
	}
	if h := hdrs["secret.key"]; h == nil || h.Mode != 0o600 || contents["secret.key"] != "k" {
		t.Fatalf("secret.key header = %+v", hdrs["secret.key"])
	}
	if h := hdrs["pkg/data.bin"]; h == nil || h.Mode != 0o640 || contents["pkg/data.bin"] != "\xca\xfe\x00" {
		t.Fatalf("pkg/data.bin header = %+v content=%q", hdrs["pkg/data.bin"], contents["pkg/data.bin"])
	}
	if h := hdrs["latest"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "run.sh" {
		t.Fatalf("symlink header = %+v", hdrs["latest"])
	}
	if h := hdrs["pkg"]; h == nil || h.Typeflag != tar.TypeDir {
		t.Fatalf("dir header = %+v", hdrs["pkg"])
	}
}

func TestContractGlobEmptyPattern(t *testing.T) {
	sb := newContractPair(t)
	matches, err := sb.Glob(t.Context(), "/workspace/*.nomatch")
	if err != nil || len(matches) != 0 {
		t.Fatalf("glob = %v err=%v, want empty", matches, err)
	}
}

// Compile-time: the sandbox really is a full Executor (beyond the
// var _ Executor = (*Sandbox)(nil) in sandbox.go, this file exercises it
// at runtime against the contract fake).
var _ fs.FileInfo = (*sandboxFileInfo)(nil)
