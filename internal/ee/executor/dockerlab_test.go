//go:build dockerlab

// Real-daemon smoke test for the docker driver: one full sandbox lifecycle
// (create hardened → exec variants → stdin → timeout ladder → kill ladder →
// CopyIn/CopyOut round-trip → list → remove). Requires a reachable Docker
// daemon (DOCKER_HOST or the default socket); without one the test skips.
// Run via `make executor-dockerlab-test`.

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	mobyclient "github.com/moby/moby/client"
)

const dockerLabImage = "alpine:latest"

// dockerLabHost picks the daemon socket explicitly (never FromEnv in the
// driver itself; the lab test honors DOCKER_HOST for OrbStack-style setups).
func dockerLabHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return defaultDockerHost
}

// requireDaemonAndImage skips when no daemon answers and makes sure the
// smoke image is present locally.
func requireDaemonAndImage(t *testing.T) context.Context {
	t.Helper()
	ctx := context.Background()
	host := dockerLabHost()
	cli, err := mobyclient.New(mobyclient.WithHost(host), mobyclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("dockerlab: client: %v", err)
	}
	if _, err := cli.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		t.Skipf("dockerlab: daemon at %s unreachable (%v); start docker and rerun", host, err)
	}
	// Pull only when the image is missing: a flaky registry must not skip
	// the smoke test when the image is already local.
	if _, err := cli.ImageInspect(ctx, dockerLabImage); err != nil {
		pull, perr := cli.ImagePull(ctx, dockerLabImage, mobyclient.ImagePullOptions{})
		if perr != nil {
			t.Skipf("dockerlab: cannot pull %s (%v)", dockerLabImage, perr)
		}
		_, _ = io.Copy(io.Discard, pull)
		_ = pull.Close()
	}
	return ctx
}

// pollUntil retries cond every 100ms up to 10s.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("dockerlab: timed out waiting for %s", what)
}

// dockerLabExec runs a sync exec and requires exit 0.
func dockerLabExec(t *testing.T, ctx context.Context, c Container, cmd ...string) ExecResult {
	t.Helper()
	res, err := c.Exec(ctx, ExecSpec{Cmd: cmd})
	if err != nil {
		t.Fatalf("dockerlab: exec %v: %v (stdout=%q stderr=%q exit=%d)", cmd, err, res.Stdout, res.Stderr, res.ExitCode)
	}
	if res.ExitCode != 0 {
		t.Fatalf("dockerlab: exec %v: exit %d (stderr=%q)", cmd, res.ExitCode, res.Stderr)
	}
	return res
}

// TestDockerLabSmoke walks the whole Backend surface against a real daemon.
func TestDockerLabSmoke(t *testing.T) {
	ctx := requireDaemonAndImage(t)
	backend, err := NewDockerBackend(dockerLabHost())
	if err != nil {
		t.Fatalf("dockerlab: backend: %v", err)
	}

	// --- full-hardening container: the main lifecycle ----------------------
	labels := map[string]string{"woca.io/test": "dockerlab", "woca.io/seat": "main"}
	full, err := backend.Create(ctx, CreateSpec{
		Image:     dockerLabImage,
		WorkDir:   "/tmp",
		Labels:    labels,
		Hardening: HardeningFull,
	})
	if err != nil {
		t.Fatalf("dockerlab: create: %v", err)
	}
	t.Cleanup(func() { _ = full.Remove(context.WithoutCancel(ctx), true) })

	// State: running, labels round-trip.
	st, err := full.State(ctx)
	if err != nil {
		t.Fatalf("dockerlab: state: %v", err)
	}
	if !st.Running {
		t.Fatal("dockerlab: fresh container not running")
	}
	if st.Labels["woca.io/test"] != "dockerlab" {
		t.Errorf("dockerlab: state labels = %v", st.Labels)
	}

	// Exec: stdout capture, exit passthrough, stderr demux.
	if res := dockerLabExec(t, ctx, full, "echo", "hello"); string(res.Stdout) != "hello\n" {
		t.Errorf("dockerlab: echo stdout = %q", res.Stdout)
	}
	res, err := full.Exec(ctx, ExecSpec{Cmd: []string{"sh", "-c", "exit 7"}})
	if err != nil || res.ExitCode != 7 {
		t.Errorf("dockerlab: exit passthrough = (%v, %d), want (nil, 7)", err, res.ExitCode)
	}
	if res := dockerLabExec(t, ctx, full, "sh", "-c", "echo out; echo err 1>&2"); string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Errorf("dockerlab: stream demux stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// Exec stdin: bytes piped in, EOF via CloseWrite.
	res, err = full.Exec(ctx, ExecSpec{Cmd: []string{"cat"}, Stdin: []byte("ping\n")})
	if err != nil {
		t.Fatalf("dockerlab: cat: %v", err)
	}
	if string(res.Stdout) != "ping\n" || res.ExitCode != 0 {
		t.Errorf("dockerlab: cat = (%q, %d)", res.Stdout, res.ExitCode)
	}

	// Full hardening bites: the read-only rootfs refuses writes outside /tmp.
	if res, _ := full.Exec(ctx, ExecSpec{Cmd: []string{"touch", "/root/locked"}}); res.ExitCode == 0 {
		t.Error("dockerlab: full hardening allowed a write to the read-only rootfs")
	}

	// Exec timeout drives the kill ladder and leaves the container alive.
	begin := time.Now()
	res, err = full.Exec(ctx, ExecSpec{Cmd: []string{"sleep", "30"}, Timeout: 3 * time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("dockerlab: sleep timeout err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > 20*time.Second {
		t.Errorf("dockerlab: kill ladder took %s; TERM should land fast", elapsed)
	}
	if st, _ := full.State(ctx); !st.Running {
		t.Fatal("dockerlab: container died from an exec timeout; ladder must not container-kill")
	}

	// CopyIn: a gzipped tar (the WorkspaceStore upload format) into the
	// workspace path — writable under full hardening only as tmpfs — then
	// read it back.
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	dockerLabTarFile(t, tw, "hello.txt", "woca-copy\n")
	dockerLabTarFile(t, tw, "sub/inner.txt", "inner\n")
	if err := tw.Close(); err != nil {
		t.Fatalf("dockerlab: tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("dockerlab: gzip close: %v", err)
	}
	if err := full.CopyIn(ctx, "/workspace/in", bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatalf("dockerlab: CopyIn: %v", err)
	}
	if res := dockerLabExec(t, ctx, full, "cat", "/workspace/in/hello.txt", "/workspace/in/sub/inner.txt"); string(res.Stdout) != "woca-copy\ninner\n" {
		t.Errorf("dockerlab: CopyIn round-trip = %q", res.Stdout)
	}
	// Plain (uncompressed) tar exercises the sniffer's other branch.
	var plainBuf bytes.Buffer
	ptw := tar.NewWriter(&plainBuf)
	dockerLabTarFile(t, ptw, "plain.txt", "flat\n")
	if err := ptw.Close(); err != nil {
		t.Fatalf("dockerlab: plain tar close: %v", err)
	}
	if err := full.CopyIn(ctx, "/workspace/plain", bytes.NewReader(plainBuf.Bytes())); err != nil {
		t.Fatalf("dockerlab: CopyIn plain: %v", err)
	}
	if res := dockerLabExec(t, ctx, full, "cat", "/workspace/plain/plain.txt"); string(res.Stdout) != "flat\n" {
		t.Errorf("dockerlab: plain CopyIn round-trip = %q", res.Stdout)
	}

	// CopyOut: docker archive naming — entries relative to the parent.
	rc, err := full.CopyOut(ctx, "/workspace/in")
	if err != nil {
		t.Fatalf("dockerlab: CopyOut: %v", err)
	}
	names, contents := readDockerLabTar(t, rc)
	if !names["in/hello.txt"] || !names["in/sub/inner.txt"] {
		t.Errorf("dockerlab: CopyOut entries = %v", names)
	}
	if contents["in/sub/inner.txt"] != "inner\n" {
		t.Errorf("dockerlab: CopyOut inner = %q", contents["in/sub/inner.txt"])
	}
	rc, err = full.CopyOut(ctx, "/workspace/in/hello.txt")
	if err != nil {
		t.Fatalf("dockerlab: CopyOut file: %v", err)
	}
	names, contents = readDockerLabTar(t, rc)
	if !names["hello.txt"] || contents["hello.txt"] != "woca-copy\n" {
		t.Errorf("dockerlab: CopyOut single file = %v %q", names, contents["hello.txt"])
	}

	// List: label match finds the container, a mismatch does not.
	found, err := backend.List(ctx, labels)
	if err != nil {
		t.Fatalf("dockerlab: list: %v", err)
	}
	if len(found) != 1 || found[0].ID() != full.ID() {
		t.Errorf("dockerlab: list by labels = %d containers, want exactly %s", len(found), full.ID())
	}
	if found, _ := backend.List(ctx, map[string]string{"woca.io/test": "nope"}); len(found) != 0 {
		t.Errorf("dockerlab: list mismatch returned %d containers", len(found))
	}

	// Minimal hardening container: same image, rootfs writable.
	minimal, err := backend.Create(ctx, CreateSpec{
		Image:     dockerLabImage,
		Labels:    map[string]string{"woca.io/test": "dockerlab", "woca.io/seat": "minimal"},
		Hardening: HardeningMinimal,
	})
	if err != nil {
		t.Fatalf("dockerlab: create minimal: %v", err)
	}
	t.Cleanup(func() { _ = minimal.Remove(context.WithoutCancel(ctx), true) })
	dockerLabExec(t, ctx, minimal, "touch", "/root/unlocked") // would fail under Full

	// --- process handle: Start, WriteStdin, Snapshot, CloseStdin ----------
	proc, err := full.Start(ctx, ExecSpec{Cmd: []string{"cat"}})
	if err != nil {
		t.Fatalf("dockerlab: start cat: %v", err)
	}
	if err := proc.WriteStdin([]byte("hi\n")); err != nil {
		t.Fatalf("dockerlab: WriteStdin: %v", err)
	}
	pollUntil(t, "cat echo", func() bool {
		events, _, _, _ := proc.Snapshot(0)
		for _, e := range events {
			if e.Stream == "stdout" && strings.Contains(string(e.Data), "hi\n") {
				return true
			}
		}
		return false
	})
	if err := proc.CloseStdin(); err != nil {
		t.Fatalf("dockerlab: CloseStdin: %v", err)
	}
	pollUntil(t, "cat EOF exit", func() bool {
		_, _, done, exit := proc.Snapshot(0)
		return done && exit == 0
	})
	if err := proc.Close(); err != nil {
		t.Errorf("dockerlab: Close after exit: %v", err)
	}

	// --- kill ladder on its own container (last resort container-kills) ----
	klabels := map[string]string{"woca.io/test": "dockerlab", "woca.io/seat": "kill"}
	kc, err := backend.Create(ctx, CreateSpec{Image: dockerLabImage, Labels: klabels, Hardening: HardeningFull})
	if err != nil {
		t.Fatalf("dockerlab: create kill-container: %v", err)
	}
	t.Cleanup(func() { _ = kc.Remove(context.WithoutCancel(ctx), true) })

	sleeper, err := kc.Start(ctx, ExecSpec{Cmd: []string{"sleep", "300"}})
	if err != nil {
		t.Fatalf("dockerlab: start sleep: %v", err)
	}
	// Wait until the wrapper's pidfile exists and its pid is alive, so the
	// ladder's first TERM has a target.
	pollUntil(t, "session pid alive", func() bool {
		res, _ := kc.Exec(ctx, ExecSpec{Cmd: []string{"sh", "-c", "kill -0 $(cat /tmp/.woca-ses-*.pid)"}})
		return res.ExitCode == 0
	})

	// Remove without force refuses while the session runs.
	if err := kc.Remove(ctx, false); err == nil {
		t.Error("dockerlab: Remove(false) succeeded while a session was running")
	}

	begin = time.Now()
	if err := sleeper.Kill(ctx); err != nil {
		t.Fatalf("dockerlab: Kill: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 3*killGrace {
		t.Errorf("dockerlab: Kill took %s; the pidfile TERM should land in the first grace window", elapsed)
	}
	pollUntil(t, "sleep killed", func() bool {
		_, _, done, _ := sleeper.Snapshot(0)
		return done
	})
	// The session died, not the container — that is the whole point of the
	// pidfile ladder (Docker has no kill-one-exec API).
	if st, _ := kc.State(ctx); !st.Running {
		t.Fatal("dockerlab: kill ladder escalated to a container kill it did not need")
	}
	if err := sleeper.Close(); err != nil {
		t.Errorf("dockerlab: Close after kill: %v", err)
	}

	// Graceful remove once no session runs; then the fake's ErrRemoved rule.
	if err := kc.Remove(ctx, false); err != nil {
		t.Fatalf("dockerlab: graceful remove: %v", err)
	}
	if _, err := kc.State(ctx); !errors.Is(err, ErrRemoved) {
		t.Errorf("dockerlab: State after remove = %v, want ErrRemoved", err)
	}
	if err := kc.Remove(ctx, true); !errors.Is(err, ErrRemoved) {
		t.Errorf("dockerlab: double remove = %v, want ErrRemoved", err)
	}
}

func dockerLabTarFile(t *testing.T, tw *tar.Writer, name, content string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatalf("dockerlab: tar header %s: %v", name, err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("dockerlab: tar write %s: %v", name, err)
	}
}

// readDockerLabTar drains a tar stream into its entry-name set and file
// contents.
func readDockerLabTar(t *testing.T, rc io.ReadCloser) (map[string]bool, map[string]string) {
	t.Helper()
	defer func() { _ = rc.Close() }()
	names := map[string]bool{}
	contents := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, contents
		}
		if err != nil {
			t.Fatalf("dockerlab: read tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			names[strings.TrimSuffix(hdr.Name, "/")] = true
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("dockerlab: read %s: %v", hdr.Name, err)
		}
		names[hdr.Name] = true
		contents[hdr.Name] = string(data)
	}
}

// TestDockerLabWorkspaceBind verifies the mount semantics (spec §5.3)
// against a real daemon: a WorkspaceDir create bind-mounts that host
// directory onto /workspace — writable even under Full hardening's
// read-only rootfs — so exec writes and CopyIn land on the host directory
// in place, CopyOut reads them back, a missing directory fails Create
// loudly, and Remove never deletes the workspace.
func TestDockerLabWorkspaceBind(t *testing.T) {
	ctx := requireDaemonAndImage(t)
	backend, err := NewDockerBackend(dockerLabHost())
	if err != nil {
		t.Fatalf("dockerlab: backend: %v", err)
	}

	ws, err := os.MkdirTemp("", "dockerlab-ws-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws+"/pre.txt", []byte("already-there"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := backend.Create(ctx, CreateSpec{
		Image:        dockerLabImage,
		WorkDir:      "/workspace",
		WorkspaceDir: ws,
		Labels:       map[string]string{"woca.io/test": "dockerlab-bind"},
		Hardening:    HardeningFull,
	})
	if err != nil {
		t.Fatalf("dockerlab: create with workspace bind: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove(context.WithoutCancel(ctx), true) })

	// The pre-existing host file is visible through the mount; a write in
	// the container lands on the host directory (read-only rootfs and
	// all — the bind is its own writable surface).
	if res := dockerLabExec(t, ctx, c, "cat", "/workspace/pre.txt"); string(res.Stdout) != "already-there" {
		t.Fatalf("dockerlab: mounted pre.txt = %q (stderr %q)", res.Stdout, res.Stderr)
	}
	dockerLabExec(t, ctx, c, "sh", "-c", "echo mounted > /workspace/product.txt")
	if b, err := os.ReadFile(ws + "/product.txt"); err != nil || string(b) != "mounted\n" {
		t.Fatalf("dockerlab: host product.txt = (%q, %v), want %q", b, err, "mounted\n")
	}

	// CopyIn unpacks onto the same directory; CopyOut reads it back.
	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	dockerLabTarFile(t, tw, "imported.txt", "delta")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.CopyIn(ctx, "/workspace", bytes.NewReader(tarbuf.Bytes())); err != nil {
		t.Fatalf("dockerlab: CopyIn into the bind: %v", err)
	}
	if b, err := os.ReadFile(ws + "/imported.txt"); err != nil || string(b) != "delta" {
		t.Fatalf("dockerlab: host imported.txt = (%q, %v)", b, err)
	}
	rc, err := c.CopyOut(ctx, "/workspace")
	if err != nil {
		t.Fatalf("dockerlab: CopyOut of the bind: %v", err)
	}
	_, contents := readDockerLabTar(t, rc)
	if contents["workspace/product.txt"] != "mounted\n" || contents["workspace/imported.txt"] != "delta" {
		t.Fatalf("dockerlab: CopyOut contents = %v", contents)
	}

	// A missing workspace directory fails Create explicitly.
	if _, err := backend.Create(ctx, CreateSpec{
		Image: dockerLabImage, WorkDir: "/workspace",
		WorkspaceDir: ws + "/nope", Hardening: HardeningFull,
	}); err == nil {
		t.Fatal("dockerlab: create with missing WorkspaceDir succeeded, want error")
	}

	// Remove reclaims the container, never the workspace directory.
	if err := c.Remove(ctx, true); err != nil {
		t.Fatalf("dockerlab: remove: %v", err)
	}
	if b, err := os.ReadFile(ws + "/product.txt"); err != nil || string(b) != "mounted\n" {
		t.Fatalf("dockerlab: workspace content after remove = (%q, %v)", b, err)
	}
}
