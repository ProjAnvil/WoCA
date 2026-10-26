//go:build dockerlab

// The real Docker driver for the Backend interface, entirely behind the
// dockerlab build tag (alongside the complab/netlab/sandboxlab lab tags):
// plain `go build ./...` compiles none of this and never imports a Docker
// package — see hardening.go and prockill.go for the docker-free halves of
// this driver (the hardening profile and the kill-ladder argv).
//
// One structural note drives prockill.go: Docker has no API to kill a single
// exec, and closing the attach connection does NOT stop the exec'd process.
// So every command this driver runs (sync Exec and Start alike) is wrapped
// by WrapArgv in a self-recording shell that publishes its pid to /tmp
// before exec'ing the real argv, and Kill signals that pidfile (KillArgv)
// with a TERM → KILL ladder whose last resort is killing the whole container.

package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

const (
	// defaultDockerHost is used when NewDockerBackend gets an empty host.
	// Deliberately explicit: the driver never reads DOCKER_HOST itself.
	defaultDockerHost = "unix:///var/run/docker.sock"

	// sandboxIdleCmd is the container's main process: sandbox containers
	// exist to host exec sessions, so the main process just idles.
	sandboxIdleCmd = "tail -f /dev/null"

	// Kill-ladder timings: TERM gets this long to land, then KILL, and only
	// after both miss does the ladder escalate to killing the container.
	killGrace = 2 * time.Second

	// execInspectInterval / execInspectTimeout bound the exit-code polling
	// after the output streams close (EOF on output != exit code ready).
	execInspectInterval = 75 * time.Millisecond
	execInspectTimeout  = 5 * time.Second

	// closeDrainTimeout bounds Close's wait for the copy loop to drain.
	closeDrainTimeout = 5 * time.Second
)

// dockerBackend drives sandbox containers through the moby client.
type dockerBackend struct {
	cli  *mobyclient.Client
	host string
}

var _ Backend = (*dockerBackend)(nil)

// NewDockerBackend connects to the Docker daemon at host (empty means the
// default unix socket). The host is explicit — never FromEnv — so callers
// (tests, the Task 8/16 wiring) own the socket the driver talks to.
func NewDockerBackend(host string) (Backend, error) {
	if host == "" {
		host = defaultDockerHost
	}
	cli, err := mobyclient.New(mobyclient.WithHost(host), mobyclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker backend: client for %s: %w", host, err)
	}
	return &dockerBackend{cli: cli, host: host}, nil
}

// Create implements Backend: the container is created with the hardening
// profile for spec.Hardening (see hardening.go) and started on its idle
// command, ready for exec sessions.
func (b *dockerBackend) Create(ctx context.Context, spec CreateSpec) (Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Image == "" {
		return nil, errors.New("docker backend: CreateSpec.Image is required")
	}
	h := hardeningSpecFor(spec.Hardening)

	// PidsLimit: the spec's explicit cap wins; unset falls back to the
	// hardening profile's default (512) — the fail-closed posture.
	pids := spec.PidsLimit
	if pids <= 0 {
		pids = h.PidsLimit
	}
	hc := &container.HostConfig{
		Runtime:        spec.Runtime,
		CapDrop:        slices.Clone(h.CapDrop),
		SecurityOpt:    slices.Clone(h.SecurityOpt),
		ReadonlyRootfs: h.ReadonlyRootfs,
		Tmpfs:          maps.Clone(h.Tmpfs),
		NetworkMode:    container.NetworkMode(spec.Network),
		// Masked/read-only /proc paths stay at engine defaults — the
		// profile never widens them (hardening.go).
	}
	// PidsLimit is a promoted field (embedded Resources); assigning it out
	// of the literal keeps go1.26 language compatibility.
	hc.PidsLimit = &pids
	if h.ReadonlyRootfs {
		// Full hardening mounts the rootfs read-only, so besides the
		// profile's /tmp the sandbox workspace path (WorkspaceStore's
		// CopyIn target) needs writable scratch too — it does not exist
		// in the images and could never be created on a read-only rootfs.
		if hc.Tmpfs == nil {
			hc.Tmpfs = map[string]string{}
		}
		hc.Tmpfs[sandboxWorkspacePath] = ""
	}
	cfg := &container.Config{
		Image:      spec.Image,
		Cmd:        []string{"sh", "-c", sandboxIdleCmd},
		WorkingDir: spec.WorkDir,
		Labels:     maps.Clone(spec.Labels),
		// Env: injected by the Task 16 options wiring, not CreateSpec.
	}
	res, err := b.cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{Config: cfg, HostConfig: hc})
	if err != nil {
		return nil, fmt.Errorf("docker backend: create %s: %w", spec.Image, err)
	}
	c := &dockerContainer{backend: b, id: res.ID, labels: maps.Clone(spec.Labels)}

	if _, err := b.cli.ContainerStart(ctx, res.ID, mobyclient.ContainerStartOptions{}); err != nil {
		// Never leak a created-but-unstarted container behind an error.
		_, _ = b.cli.ContainerRemove(context.WithoutCancel(ctx), res.ID, mobyclient.ContainerRemoveOptions{Force: true})
		return nil, fmt.Errorf("docker backend: start %s: %w", res.ID, err)
	}
	return c, nil
}

// List implements Backend: every container whose labels match all given
// pairs, in ID order.
func (b *dockerBackend) List(ctx context.Context, labels map[string]string) ([]Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filters := make(mobyclient.Filters)
	for k, v := range labels {
		filters.Add("label", k+"="+v)
	}
	res, err := b.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("docker backend: list: %w", err)
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, &dockerContainer{backend: b, id: s.ID, labels: maps.Clone(s.Labels)})
	}
	slices.SortFunc(out, func(a, b Container) int { return strings.Compare(a.ID(), b.ID()) })
	return out, nil
}

// --- docker container -------------------------------------------------------

// dockerContainer is the driver's Container: a started sandbox container
// plus the session processes it hosts.
type dockerContainer struct {
	backend *dockerBackend
	mu      sync.Mutex
	id      string
	labels  map[string]string
	procs   []*dockerProcess
	removed bool
}

var _ Container = (*dockerContainer)(nil)

func (c *dockerContainer) ID() string { return c.id }

func (c *dockerContainer) shortID() string {
	if len(c.id) <= 12 {
		return c.id
	}
	return c.id[:12]
}

// State implements Container from the engine's own view.
func (c *dockerContainer) State(ctx context.Context) (State, error) {
	res, err := c.backend.cli.ContainerInspect(ctx, c.id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return State{}, c.wrapGone("inspect", err)
	}
	st := State{Labels: maps.Clone(c.labels)}
	if s := res.Container.State; s != nil {
		st.Running = s.Running
		st.ExitCode = s.ExitCode
	}
	return st, nil
}

// CopyIn implements Container by streaming the tarball through an in-container
// `tar -x`. The engine's own archive API (PUT /archive) cannot be used: it
// refuses read-only-rootfs containers outright and cannot see tmpfs mounts —
// and under full hardening every writable path IS tmpfs. Gzip streams are
// sniffed (WorkspaceStore uploads tar.gz) and handed to `tar -xz`.
func (c *dockerContainer) CopyIn(ctx context.Context, dstDir string, tarball io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tarball == nil {
		return fmt.Errorf("docker container %s: CopyIn tarball is nil", c.shortID())
	}
	dst := path.Clean("/" + dstDir)
	if err := c.runQuiet(ctx, []string{"mkdir", "-p", dst}); err != nil {
		return fmt.Errorf("docker container %s: CopyIn mkdir %s: %w", c.shortID(), dstDir, err)
	}

	cmd := []string{"tar", "-x", "-C", dst}
	br := bufio.NewReader(tarball)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		cmd = []string{"tar", "-xz", "-C", dst}
	}
	if err := c.pipeTo(ctx, cmd, br); err != nil {
		return fmt.Errorf("docker container %s: CopyIn %s: %w", c.shortID(), dstDir, err)
	}
	return nil
}

// CopyOut implements Container by producing the tar inside the container
// (`tar -c -C <parent> <base>`) and streaming it back over the exec's
// stdout — same reason as CopyIn: the engine's archive API cannot read
// tmpfs paths. Entries land relative to the path's parent, per the
// Container interface contract (Docker archive naming). The caller closes
// the stream.
func (c *dockerContainer) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cleaned := path.Clean("/" + p)
	proc, err := c.startSession(ctx, ExecSpec{Cmd: []string{"tar", "-c", "-C", path.Dir(cleaned), path.Base(cleaned)}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = proc.Close() }()
	if err := proc.CloseStdin(); err != nil {
		return nil, fmt.Errorf("docker container %s: CopyOut %s: %w", c.shortID(), p, err)
	}
	if err := proc.waitDone(ctx, 0); err != nil {
		return nil, fmt.Errorf("docker container %s: CopyOut %s: %w", c.shortID(), p, err)
	}
	stdout, stderr, exit := proc.aggregate()
	if exit != 0 {
		return nil, fmt.Errorf("docker container %s: CopyOut %s: tar exit %d (%s)", c.shortID(), p, exit, bytes.TrimSpace(stderr))
	}
	return io.NopCloser(bytes.NewReader(stdout)), nil
}

// pipeTo runs argv with r streamed to its stdin and requires exit 0.
func (c *dockerContainer) pipeTo(ctx context.Context, argv []string, r io.Reader) error {
	proc, err := c.startSession(ctx, ExecSpec{Cmd: argv})
	if err != nil {
		return err
	}
	defer func() { _ = proc.Close() }()
	if err := proc.writeStream(r); err != nil {
		return err
	}
	if err := proc.CloseStdin(); err != nil {
		return err
	}
	if err := proc.waitDone(ctx, 0); err != nil {
		return err
	}
	if _, stderr, exit := proc.aggregate(); exit != 0 {
		return fmt.Errorf("exit status %d (%s)", exit, bytes.TrimSpace(stderr))
	}
	return nil
}

// Exec implements Container with the same semantics as the fake: exit codes
// pass through as data; only start failures, deadline kills, and
// cancellations are errors (then the result still carries drained output).
func (c *dockerContainer) Exec(ctx context.Context, req ExecSpec) (ExecResult, error) {
	res := ExecResult{ExitCode: -1}
	if len(req.Cmd) == 0 {
		return res, errors.New("docker container: ExecSpec.Cmd must not be empty")
	}
	p, err := c.startSession(ctx, req)
	if err != nil {
		return res, err
	}
	defer func() { _ = p.Close() }()

	if len(req.Stdin) > 0 {
		if err := p.WriteStdin(req.Stdin); err != nil {
			return res, err
		}
	}
	if err := p.CloseStdin(); err != nil {
		return res, err
	}

	if err := p.waitDone(ctx, req.Timeout); err != nil {
		res.Stdout, res.Stderr, res.ExitCode = p.aggregate()
		return res, fmt.Errorf("docker container %s: exec %q: %w", c.shortID(), req.Cmd[0], err)
	}
	res.Stdout, res.Stderr, res.ExitCode = p.aggregate()
	if res.ExitCode == -1 {
		return res, fmt.Errorf("docker container %s: exec %q: no exit status", c.shortID(), req.Cmd[0])
	}
	return res, nil
}

// Start implements Container: the session runs until killed through the
// Process handle (or its container dies); output is captured into the event
// log read via Snapshot. ExecSpec.Stdin and ExecSpec.Timeout are ignored.
func (c *dockerContainer) Start(ctx context.Context, req ExecSpec) (Process, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("docker container: ExecSpec.Cmd must not be empty")
	}
	p, err := c.startSession(ctx, req)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed {
		_ = p.Close()
		return nil, fmt.Errorf("docker container %s: %w", c.shortID(), ErrRemoved)
	}
	c.procs = append(c.procs, p)
	return p, nil
}

// Remove implements Container. Without force it fails while a session is
// still running; the engine also refuses to remove our always-idling
// container while it runs, so the graceful path stops it first.
func (c *dockerContainer) Remove(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.removed {
		c.mu.Unlock()
		return fmt.Errorf("docker container %s: %w", c.shortID(), ErrRemoved)
	}
	if !force {
		for _, p := range c.procs {
			if !p.isDone() {
				c.mu.Unlock()
				return fmt.Errorf("docker container %s: session still running (use force)", c.shortID())
			}
		}
	}
	c.removed = true
	procs := c.procs
	c.procs = nil
	c.mu.Unlock()

	if force {
		if _, err := c.backend.cli.ContainerRemove(ctx, c.id, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return c.wrapGone("remove", err)
		}
	} else {
		if _, err := c.backend.cli.ContainerStop(ctx, c.id, mobyclient.ContainerStopOptions{}); err != nil {
			return c.wrapGone("stop", err)
		}
		if _, err := c.backend.cli.ContainerRemove(ctx, c.id, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return c.wrapGone("remove", err)
		}
	}
	// The engine killed every session with the container; reap handles.
	for _, p := range procs {
		_ = p.Close()
	}
	return nil
}

// startSession wraps req.Cmd with the pidfile shim, creates and attaches a
// Docker exec, and starts the output capture goroutine.
func (c *dockerContainer) startSession(ctx context.Context, req ExecSpec) (*dockerProcess, error) {
	sid, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("docker container %s: session id: %w", c.shortID(), err)
	}
	argv := WrapArgv(sid, req.Cmd)
	if argv == nil {
		return nil, fmt.Errorf("docker container %s: unsafe session command", c.shortID())
	}
	created, err := c.backend.cli.ExecCreate(ctx, c.id, mobyclient.ExecCreateOptions{
		Cmd:          argv,
		WorkingDir:   req.Dir, // empty means the container's WorkDir
		Env:          req.Env,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          false,
	})
	if err != nil {
		return nil, c.wrapGone("exec create", err)
	}
	attached, err := c.backend.cli.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{TTY: false})
	if err != nil {
		return nil, c.wrapGone("exec attach", err)
	}

	p := &dockerProcess{
		container: c,
		execID:    created.ID,
		sid:       sid,
		resp:      attached,
		finished:  make(chan struct{}),
	}
	go p.copyLoop()
	return p, nil
}

// runQuiet runs one no-attach exec (used for mkdir and kill signals) and
// waits for it to finish.
func (c *dockerContainer) runQuiet(ctx context.Context, argv []string) error {
	created, err := c.backend.cli.ExecCreate(ctx, c.id, mobyclient.ExecCreateOptions{
		Cmd: argv,
		TTY: false,
	})
	if err != nil {
		return c.wrapGone("exec create", err)
	}
	if _, err := c.backend.cli.ExecStart(ctx, created.ID, mobyclient.ExecStartOptions{}); err != nil {
		return c.wrapGone("exec start", err)
	}
	deadline := time.Now().Add(execInspectTimeout)
	for {
		insp, err := c.backend.cli.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
		if err != nil {
			return c.wrapGone("exec inspect", err)
		}
		if !insp.Running {
			if insp.ExitCode != 0 {
				return fmt.Errorf("exit status %d", insp.ExitCode)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		time.Sleep(execInspectInterval)
	}
}

// signalSession runs KillArgv(sid, sig) in the container — the pidfile kill
// from prockill.go.
func (c *dockerContainer) signalSession(ctx context.Context, sid, sig string) error {
	argv := KillArgv(sid, sig)
	if argv == nil {
		return fmt.Errorf("docker container %s: unsafe kill (sid/sig rejected)", c.shortID())
	}
	return c.runQuiet(ctx, argv)
}

// wrapGone maps engine 404s to ErrRemoved (the fake's post-Remove error)
// and wraps everything else with container context.
func (c *dockerContainer) wrapGone(op string, err error) error {
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("docker container %s: %s: %w", c.shortID(), op, ErrRemoved)
	}
	return fmt.Errorf("docker container %s: %s: %w", c.shortID(), op, err)
}

// newSessionID mints a random, pidfile-safe session id (hex; within
// [0-9A-Za-z] so WrapArgv/KillArgv always accept it).
func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// --- docker process ---------------------------------------------------------

// dockerProcess is the driver's Process: one attached Docker exec whose
// output is demultiplexed into the same append-only event log the fake
// uses (cumulative offsets across both streams, Snapshot(offset)).
type dockerProcess struct {
	container *dockerContainer
	execID    string
	sid       string

	mu          sync.Mutex
	resp        mobyclient.ExecAttachResult
	stdinClosed bool
	closed      bool
	events      []OutputEvent
	total       int
	done        bool
	exit        int

	writeMu  sync.Mutex    // serializes stdin writes on the hijacked conn
	finished chan struct{} // closed once done/exit are final
}

var _ Process = (*dockerProcess)(nil)

// WriteStdin implements Process: raw bytes on the hijacked connection
// (stdin is unframed client→daemon; only the output direction is
// multiplexed). Writes are serialized; the write happens outside p.mu so a
// full pipe cannot block Close/Kill.
func (p *dockerProcess) WriteStdin(b []byte) error {
	p.mu.Lock()
	closed := p.closed || p.stdinClosed
	p.mu.Unlock()
	if closed {
		return errors.New("docker process: stdin closed")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.resp.Conn.Write(b); err != nil {
		return fmt.Errorf("docker process: stdin write: %w", err)
	}
	return nil
}

// writeStream copies all of r to the process's stdin under the write lock
// (CopyIn's streaming path).
func (p *dockerProcess) writeStream(r io.Reader) error {
	p.mu.Lock()
	closed := p.closed || p.stdinClosed
	p.mu.Unlock()
	if closed {
		return errors.New("docker process: stdin closed")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := io.Copy(p.resp.Conn, r); err != nil {
		return fmt.Errorf("docker process: stdin stream: %w", err)
	}
	return nil
}

// CloseStdin implements Process: closes only the write half of the
// hijacked connection (EOF to the process) so the output direction stays
// open until every byte is drained. Idempotent.
func (p *dockerProcess) CloseStdin() error {
	p.mu.Lock()
	if p.stdinClosed {
		p.mu.Unlock()
		return nil
	}
	p.stdinClosed = true
	p.mu.Unlock()

	if err := p.resp.CloseWrite(); err != nil {
		return fmt.Errorf("docker process: stdin close: %w", err)
	}
	return nil
}

// Snapshot implements Process, with the fake's exact semantics: events at
// or after offset, the next offset, and done only after the output is fully
// drained and the exit code is known.
func (p *dockerProcess) Snapshot(offset int) ([]OutputEvent, int, bool, int) {
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

// Kill implements Process via the ladder from prockill.go: TERM to the
// recorded pid, ~2s grace, KILL to the pid, ~2s grace, and only then —
// because nothing else is left — SIGKILL the whole container (which kills
// the container's other sessions too; the last resort is deliberately
// destructive). Killing an already-exited process is a nil error.
func (p *dockerProcess) Kill(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.isDone() {
		return nil
	}

	for _, sig := range []string{"TERM", "KILL"} {
		// Best effort: the pidfile may not exist (already reaped) or the
		// signal exec may lose a race with the process exiting.
		_ = p.container.signalSession(ctx, p.sid, sig)
		if p.waitFinished(killGrace) {
			return nil
		}
	}
	if _, err := p.container.backend.cli.ContainerKill(ctx, p.container.id, mobyclient.ContainerKillOptions{Signal: "SIGKILL"}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("docker process: container kill: %w", err)
	}
	if p.waitFinished(killGrace) {
		return nil
	}
	return errors.New("docker process: kill ladder exhausted")
}

// Close implements Process: close stdin, kill whatever is still running
// (nothing may leak past the caller), then wait for the output drain.
// Idempotent.
func (p *dockerProcess) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	done := p.done
	p.mu.Unlock()

	_ = p.CloseStdin()
	if !done {
		// Kill ladder on a fresh ctx: the caller's ctx may already be done.
		_ = p.Kill(context.Background())
	}
	timedOut := false
	select {
	case <-p.finished:
	case <-time.After(closeDrainTimeout):
		timedOut = true
	}
	p.resp.Close()
	if timedOut {
		return errors.New("docker process: close timed out")
	}
	return nil
}

// isDone reports whether exit status is published.
func (p *dockerProcess) isDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// waitFinished reports whether the process finished within d.
func (p *dockerProcess) waitFinished(d time.Duration) bool {
	select {
	case <-p.finished:
		return true
	case <-time.After(d):
		return false
	}
}

// waitDone blocks until the process finishes, the timeout elapses, or ctx
// is cancelled; on timeout/cancel it runs the kill ladder first so the
// caller still receives every drained output byte.
func (p *dockerProcess) waitDone(ctx context.Context, timeout time.Duration) error {
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	for {
		select {
		case <-p.finished:
			return nil
		case <-ctx.Done():
			_ = p.Kill(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-timer:
			_ = p.Kill(context.Background())
			return context.DeadlineExceeded
		}
	}
}

// aggregate concatenates the captured events into full stdout/stderr and
// returns them with the exit code (Exec's synchronous view).
func (p *dockerProcess) aggregate() ([]byte, []byte, int) {
	events, _, _, exit := p.Snapshot(0)
	var stdout, stderr []byte
	for _, e := range events {
		switch e.Stream {
		case "stdout":
			stdout = append(stdout, e.Data...)
		case "stderr":
			stderr = append(stderr, e.Data...)
		}
	}
	return stdout, stderr, exit
}

// copyLoop demultiplexes the hijacked connection into the event log until
// the streams close, then polls ExecInspect for the exit code — StdCopy
// hitting EOF does not mean the status is ready yet — and publishes
// done/exit last so a done Snapshot always carries all output.
func (p *dockerProcess) copyLoop() {
	stdoutW := &eventWriter{p: p, stream: "stdout"}
	stderrW := &eventWriter{p: p, stream: "stderr"}
	_, _ = stdcopy.StdCopy(stdoutW, stderrW, p.resp.Reader)

	exit, ok := p.pollExit()
	p.mu.Lock()
	if ok {
		p.exit = exit
	} else {
		p.exit = -1
	}
	p.done = true
	p.mu.Unlock()
	close(p.finished)
}

// pollExit polls ExecInspect until the exec stops Running (or the poll
// budget runs out). It uses context.Background: the session's own ctx may
// be the reason the process is dying.
func (p *dockerProcess) pollExit() (int, bool) {
	deadline := time.Now().Add(execInspectTimeout)
	for {
		insp, err := p.container.backend.cli.ExecInspect(context.Background(), p.execID, mobyclient.ExecInspectOptions{})
		if err == nil && !insp.Running {
			return insp.ExitCode, true
		}
		if time.Now().After(deadline) {
			return -1, false
		}
		time.Sleep(execInspectInterval)
	}
}

// eventWriter appends to a dockerProcess's event log under one stream tag.
type eventWriter struct {
	p      *dockerProcess
	stream string
}

func (w *eventWriter) Write(b []byte) (int, error) {
	chunk := bytes.Clone(b)
	w.p.mu.Lock()
	w.p.events = append(w.p.events, OutputEvent{Stream: w.stream, Data: chunk, Start: w.p.total})
	w.p.total += len(chunk)
	w.p.mu.Unlock()
	return len(b), nil
}
