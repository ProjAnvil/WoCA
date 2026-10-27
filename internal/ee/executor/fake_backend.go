package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrRemoved is returned by every fake-container operation after Remove
// (and by Remove itself on a second call).
var ErrRemoved = errors.New("executor: container removed")

// FakeBackend is an in-process Backend for tests: every container is a
// temporary directory on the host plus real os/exec processes, so the
// sandbox services can be unit-tested without any Docker daemon ("sh" must
// be on PATH). Container-side paths map onto the container's directory;
// commands see the host paths, so tests must assert on files and output
// rather than on printed paths.
type FakeBackend struct {
	// mu guards the registry itself; each FakeContainer has its own lock
	// for its state, so operations on different containers do not contend.
	mu         sync.Mutex
	root       string
	nextID     int64
	containers map[string]*FakeContainer
	closed     bool
}

var _ Backend = (*FakeBackend)(nil)

// NewFakeBackend creates a FakeBackend backed by a fresh temp directory.
// Close removes the directory and every container in it.
func NewFakeBackend() (*FakeBackend, error) {
	root, err := os.MkdirTemp("", "fake-executor-*")
	if err != nil {
		return nil, fmt.Errorf("fake executor: root temp dir: %w", err)
	}
	return &FakeBackend{root: root, containers: map[string]*FakeContainer{}}, nil
}

// Create implements Backend. The image itself is not pulled or run — it is
// only required so that spec drift from the real backend shows up in
// tests; labels, workdir and the workspace bind are honored. The fake's
// "mount" of spec.WorkspaceDir is direct: the container's /workspace path
// maps onto that host directory, so CopyIn/CopyOut and exec working
// directories read and write the real files in place (spec §5.3).
func (b *FakeBackend) Create(ctx context.Context, spec CreateSpec) (Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Image == "" {
		return nil, errors.New("fake executor: CreateSpec.Image is required")
	}
	if spec.WorkspaceDir != "" {
		info, err := os.Stat(spec.WorkspaceDir)
		if err != nil {
			return nil, fmt.Errorf("fake executor: workspace dir %s: %w", spec.WorkspaceDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("fake executor: workspace dir %s: not a directory", spec.WorkspaceDir)
		}
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("fake executor: backend closed")
	}
	b.nextID++
	id := "fake_" + strconv.FormatInt(b.nextID, 10)
	b.mu.Unlock()

	dir := filepath.Join(b.root, id)
	workDir := path.Clean("/" + spec.WorkDir)
	// The container's own root always exists (commands with a Dir outside
	// the mount chdir into it); the working directory exists on the mount
	// when it sits at or under the mounted /workspace, so only the
	// unmounted case needs a directory created inside the root.
	workDirMounted := spec.WorkspaceDir != "" &&
		(workDir == sandboxWorkspacePath || strings.HasPrefix(workDir, sandboxWorkspacePath+"/"))
	mkTarget := dir
	if !workDirMounted {
		mkTarget = filepath.Join(dir, filepath.FromSlash(workDir))
	}
	if err := os.MkdirAll(mkTarget, 0o755); err != nil {
		return nil, fmt.Errorf("fake executor: container dir for %s: %w", id, err)
	}
	c := &FakeContainer{
		backend:      b,
		id:           id,
		dir:          dir,
		workDir:      workDir,
		workspaceDir: spec.WorkspaceDir,
		labels:       maps.Clone(spec.Labels),
	}
	if c.labels == nil {
		c.labels = map[string]string{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		_ = os.RemoveAll(dir)
		return nil, errors.New("fake executor: backend closed")
	}
	b.containers[id] = c
	return c, nil
}

// List implements Backend: containers whose labels match every given
// pair, in ID order. An empty filter lists everything.
func (b *FakeBackend) List(ctx context.Context, labels map[string]string) ([]Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	containers := make([]*FakeContainer, 0, len(b.containers))
	for _, c := range b.containers {
		containers = append(containers, c)
	}
	b.mu.Unlock()

	slices.SortFunc(containers, func(a, b *FakeContainer) int { return strings.Compare(a.id, b.id) })
	out := make([]Container, 0, len(containers))
	for _, c := range containers {
		if c.matchesLabels(labels) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Close force-removes every container and deletes the backend's root
// directory. It is idempotent.
func (b *FakeBackend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	containers := make([]*FakeContainer, 0, len(b.containers))
	for _, c := range b.containers {
		containers = append(containers, c)
	}
	b.containers = map[string]*FakeContainer{}
	b.mu.Unlock()

	for _, c := range containers {
		c.hardRemove()
	}
	if err := os.RemoveAll(b.root); err != nil {
		return fmt.Errorf("fake executor: remove root: %w", err)
	}
	return nil
}

func (b *FakeBackend) deregister(id string) {
	b.mu.Lock()
	delete(b.containers, id)
	b.mu.Unlock()
}

// --- fake container ---------------------------------------------------------

// FakeContainer is the fake's Container: a host directory standing in for
// the container filesystem plus the processes started inside it. A
// non-empty workspaceDir is the fake's bind mount: container paths at or
// under /workspace resolve onto it instead of the container's own
// directory, and Remove never deletes it (it belongs to the WorkspaceStore,
// not the container).
type FakeContainer struct {
	backend      *FakeBackend
	mu           sync.Mutex
	id           string
	dir          string // host directory backing the container filesystem
	workspaceDir string // host directory "mounted" at /workspace; "" when unset
	workDir      string // container-side default working directory
	labels       map[string]string
	sessions     []*FakeProcess
	removed      bool
}

var _ Container = (*FakeContainer)(nil)

// ID implements Container.
func (c *FakeContainer) ID() string { return c.id }

// State implements Container. The fake has no main process, so Running
// reports whether any started session is still alive, and ExitCode is the
// exit status of the most recently finished session.
func (c *FakeContainer) State(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed {
		return State{}, fmt.Errorf("fake container %s: %w", c.id, ErrRemoved)
	}
	state := State{Labels: maps.Clone(c.labels)}
	for _, s := range c.sessions {
		if done, exit := s.status(); done {
			state.ExitCode = exit
		} else {
			state.Running = true
		}
	}
	return state, nil
}

// CopyIn implements Container: the tarball may be a plain tar or
// gzip-compressed tar; entries land under dstDir.
func (c *FakeContainer) CopyIn(ctx context.Context, dstDir string, tarball io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.checkLive(); err != nil {
		return err
	}
	if tarball == nil {
		return fmt.Errorf("fake container %s: CopyIn tarball is nil", c.id)
	}

	// Sniff the gzip magic so both plain and compressed tars are accepted.
	br := bufio.NewReader(tarball)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("fake container %s: CopyIn gzip: %w", c.id, err)
		}
		defer func() { _ = gz.Close() }() // read-side: the error carries nothing actionable
		src = gz
	}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("fake container %s: CopyIn tar: %w", c.id, err)
		}
		name := path.Clean(hdr.Name)
		if name == "." || name == "/" {
			continue
		}
		target := c.hostPath(path.Join(dstDir, name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, tarPerm(hdr.Mode, 0o755)); err != nil {
				return fmt.Errorf("fake container %s: CopyIn mkdir %s: %w", c.id, name, err)
			}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("fake container %s: CopyIn read %s: %w", c.id, name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("fake container %s: CopyIn mkdir for %s: %w", c.id, name, err)
			}
			if err := os.WriteFile(target, data, tarPerm(hdr.Mode, 0o644)); err != nil {
				return fmt.Errorf("fake container %s: CopyIn write %s: %w", c.id, name, err)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("fake container %s: CopyIn mkdir for %s: %w", c.id, name, err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("fake container %s: CopyIn symlink %s: %w", c.id, name, err)
			}
		default:
			// Other entry types (devices, fifos, hardlinks) are skipped.
		}
	}
}

// CopyOut implements Container, producing a plain tar stream named per
// Docker archive conventions (entries under the base name of path).
func (c *FakeContainer) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.checkLive(); err != nil {
		return nil, err
	}

	host := c.hostPath(p)
	info, err := os.Stat(host)
	if err != nil {
		return nil, fmt.Errorf("fake container %s: CopyOut %s: %w", c.id, p, err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarTree(tw, path.Base(path.Clean("/"+p)), host, info); err != nil {
		return nil, fmt.Errorf("fake container %s: CopyOut %s: %w", c.id, p, err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("fake container %s: CopyOut %s: %w", c.id, p, err)
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// Exec implements Container with real os/exec processes: exit codes pass
// through as data, and only start failures or deadline kills are errors.
func (c *FakeContainer) Exec(ctx context.Context, req ExecSpec) (ExecResult, error) {
	res := ExecResult{ExitCode: -1}
	if len(req.Cmd) == 0 {
		return res, errors.New("fake container: ExecSpec.Cmd must not be empty")
	}
	if err := c.checkLive(); err != nil {
		return res, err
	}

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = c.workHostPath(req.Dir)
	cmd.Env = mergedEnv(req.Env)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	res = ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if runCtx.Err() == nil {
			return res, nil // plain non-zero exit: passthrough, not an error
		}
		return res, fmt.Errorf("fake container %s: exec %q: %w", c.id, req.Cmd[0], runCtx.Err())
	}
	return res, fmt.Errorf("fake container %s: exec %q: %w", c.id, req.Cmd[0], err)
}

// Start implements Container. The process runs under ctx (canceling ctx
// kills it), output is captured into an event log read via Snapshot, and
// the process must eventually be stopped with Kill or Close.
func (c *FakeContainer) Start(ctx context.Context, req ExecSpec) (Process, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("fake container: ExecSpec.Cmd must not be empty")
	}
	if err := c.checkLive(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = c.workHostPath(req.Dir)
	cmd.Env = mergedEnv(req.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fake container %s: stdin pipe: %w", c.id, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("fake container %s: stdout pipe: %w", c.id, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("fake container %s: stderr pipe: %w", c.id, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fake container %s: start %q: %w", c.id, req.Cmd[0], err)
	}

	p := &FakeProcess{cmd: cmd, stdin: stdin, finished: make(chan struct{})}
	p.copyWG.Add(2)
	go p.copyEvents("stdout", stdout)
	go p.copyEvents("stderr", stderr)
	go p.reap()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("fake container %s: %w", c.id, ErrRemoved)
	}
	c.sessions = append(c.sessions, p)
	return p, nil
}

// Remove implements Container. Without force it fails while a session is
// still alive; with force it kills the sessions first. Either way the
// backing directory is deleted and later calls fail with ErrRemoved.
func (c *FakeContainer) Remove(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.removed {
		c.mu.Unlock()
		return fmt.Errorf("fake container %s: %w", c.id, ErrRemoved)
	}
	sessions := slices.Clone(c.sessions)
	if !force {
		for _, s := range sessions {
			if done, _ := s.status(); !done {
				c.mu.Unlock()
				return fmt.Errorf("fake container %s: session still running (use force)", c.id)
			}
		}
	}
	c.removed = true
	c.mu.Unlock()

	c.killSessions(sessions)
	c.backend.deregister(c.id)
	if err := os.RemoveAll(c.dir); err != nil {
		return fmt.Errorf("fake container %s: remove dir: %w", c.id, err)
	}
	return nil
}

// hardRemove is the Close path: force-kill and delete without errors.
func (c *FakeContainer) hardRemove() {
	c.mu.Lock()
	if c.removed {
		c.mu.Unlock()
		return
	}
	c.removed = true
	sessions := slices.Clone(c.sessions)
	c.mu.Unlock()

	c.killSessions(sessions)
	_ = os.RemoveAll(c.dir)
}

func (c *FakeContainer) killSessions(sessions []*FakeProcess) {
	for _, s := range sessions {
		_ = s.Close() // kills if still running and waits for drain
	}
}

func (c *FakeContainer) checkLive() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed {
		return fmt.Errorf("fake container %s: %w", c.id, ErrRemoved)
	}
	return nil
}

func (c *FakeContainer) matchesLabels(want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range want {
		if got, ok := c.labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// hostPath maps a container-side path onto the host directory backing the
// container. The path is cleaned against "/" first, so ".." cannot escape.
// When a workspace dir is "mounted", paths at or under /workspace resolve
// onto that directory — the fake's stand-in for the bind mount (Create).
func (c *FakeContainer) hostPath(p string) string {
	clean := path.Clean("/" + p)
	if c.workspaceDir != "" {
		if clean == sandboxWorkspacePath {
			return c.workspaceDir
		}
		if strings.HasPrefix(clean, sandboxWorkspacePath+"/") {
			rel := strings.TrimPrefix(clean, sandboxWorkspacePath+"/")
			return filepath.Join(c.workspaceDir, filepath.FromSlash(rel))
		}
	}
	return filepath.Join(c.dir, filepath.FromSlash(clean))
}

// workHostPath resolves the working directory for a command: req.Dir when
// set, the container's WorkDir otherwise.
func (c *FakeContainer) workHostPath(dir string) string {
	if dir == "" {
		dir = c.workDir
	}
	return c.hostPath(dir)
}

// writeTarTree appends info's entry (recursively for directories) to tw,
// named relative to the parent of the copied path.
func writeTarTree(tw *tar.Writer, name, host string, info os.FileInfo) error {
	hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
	switch {
	case info.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		entries, err := os.ReadDir(host)
		if err != nil {
			return err
		}
		for _, e := range entries { // ReadDir is sorted: deterministic output
			entryInfo, err := e.Info()
			if err != nil {
				return err
			}
			if err := writeTarTree(tw, path.Join(name, e.Name()), filepath.Join(host, e.Name()), entryInfo); err != nil {
				return err
			}
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(host)
		if err != nil {
			return err
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = link
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		return nil
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = info.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(host)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read-only handle: nothing actionable
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		return nil
	}
}

// tarPerm picks the permission bits from a tar header, falling back to
// def when the header carries none.
func tarPerm(mode int64, def os.FileMode) os.FileMode {
	if perm := os.FileMode(mode).Perm(); perm != 0 {
		return perm
	}
	return def
}

// mergedEnv overlays "K=V" entries on the backend process environment so
// PATH stays available to spawned commands.
func mergedEnv(overlay []string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, kv := range overlay {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// --- fake process -----------------------------------------------------------

// FakeProcess is the fake's Process: a real child process whose output is
// captured into an append-only event log with cumulative offsets.
type FakeProcess struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	events   []OutputEvent
	total    int
	done     bool
	exit     int
	closed   bool
	copyWG   sync.WaitGroup
	finished chan struct{}
}

var _ Process = (*FakeProcess)(nil)

// WriteStdin implements Process. The pipe write deliberately happens
// outside p.mu: a child that stops reading stdin fills the pipe buffer and
// parks the write, and holding the lock across it would block
// Close/Kill/CloseStdin forever, defeating Close's 5s safety net.
func (p *FakeProcess) WriteStdin(b []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("fake process: closed")
	}
	w := p.stdin
	p.mu.Unlock()
	if w == nil {
		return errors.New("fake process: stdin already closed")
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("fake process: stdin write: %w", err)
	}
	return nil
}

// CloseStdin implements Process; idempotent.
func (p *FakeProcess) CloseStdin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return nil
	}
	w := p.stdin
	p.stdin = nil
	if err := w.Close(); err != nil {
		return fmt.Errorf("fake process: stdin close: %w", err)
	}
	return nil
}

// Snapshot implements Process: events whose Start is at or after offset,
// the next offset to poll with, and the exit code once done. done is only
// set after the captured output is fully drained.
func (p *FakeProcess) Snapshot(offset int) ([]OutputEvent, int, bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := make([]OutputEvent, 0, len(p.events))
	for _, e := range p.events {
		if e.Start < offset {
			continue
		}
		events = append(events, OutputEvent{Stream: e.Stream, Data: bytes.Clone(e.Data), Start: e.Start})
	}
	return events, p.total, p.done, p.exit
}

// Kill implements Process; killing an already-exited process is nil.
func (p *FakeProcess) Kill(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	proc := p.cmd.Process
	p.mu.Unlock()
	if proc == nil {
		return nil
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("fake process: kill: %w", err)
	}
	return nil
}

// Close implements Process: close stdin, kill whatever is still running
// (no process may leak past the caller), then wait for the output drain
// and the reaper to finish. Idempotent.
func (p *FakeProcess) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stdin, proc := p.stdin, p.cmd.Process
	p.stdin = nil
	p.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if proc != nil {
		_ = proc.Kill() // already-exited is fine; the reaper still reaps
	}
	select {
	case <-p.finished:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("fake process: close timed out")
	}
}

// status reports (done, exitCode) atomically.
func (p *FakeProcess) status() (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.exit
}

// copyEvents drains one output pipe into the event log until EOF.
func (p *FakeProcess) copyEvents(stream string, r io.Reader) {
	defer p.copyWG.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := bytes.Clone(buf[:n])
			p.mu.Lock()
			p.events = append(p.events, OutputEvent{Stream: stream, Data: chunk, Start: p.total})
			p.total += len(chunk)
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// reap waits for both pipes to drain, then for the process, and finally
// publishes done/exit — so a done Snapshot always carries all output.
func (p *FakeProcess) reap() {
	p.copyWG.Wait()
	err := p.cmd.Wait()
	exit := 0
	if err != nil {
		exit = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
			exit = exitErr.ProcessState.ExitCode()
		}
	}
	p.mu.Lock()
	p.stdin = nil
	p.exit = exit
	p.done = true
	p.mu.Unlock()
	close(p.finished)
}
