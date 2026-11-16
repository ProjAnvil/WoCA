package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// grepScanBuffer is the maximum line size Grep can scan.
const grepScanBuffer = 1 << 20 // 1 MiB

// KillGrace is the time the local executor's Kill gives a terminated
// process group to react to SIGTERM before escalating to SIGKILL. It is
// exported because the escalation runs on a timer inside this process:
// shutdown callers that terminate background trees must outlive the timer
// (BackgroundShells.KillAll waits it out) or the escalation is lost the
// moment they exit.
const KillGrace = 2 * time.Second

// pipeWaitDelay bounds how long Wait stays blocked after the process itself
// has exited (or been killed) while a surviving grandchild still holds the
// output pipes: os/exec then closes the pipes and Wait returns instead of
// hanging forever. It must exceed KillGrace so a process that ignores
// SIGTERM is ended by the SIGKILL escalation, not by the pipe deadline.
const pipeWaitDelay = KillGrace + time.Second

// Local is the single-machine Executor implementation backed by os/exec
// and the local filesystem. It holds no state and is safe for concurrent use.
type Local struct{}

// NewLocal returns a Local executor.
func NewLocal() *Local { return &Local{} }

// Exec runs the command locally via exec.CommandContext.
func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = req.Env
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		// The process ran and exited non-zero: report the code, not an error.
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	// Startup failure or context cancellation.
	return res, err
}

// localProcess adapts an *exec.Cmd started by Local.Start.
type localProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader // nil when redirected via StartOptions.Stdout

	// exited is closed once Wait has returned, letting a pending SIGTERM
	// escalation (see terminate) stand down because the process is gone.
	exited     chan struct{}
	exitedOnce sync.Once
	termOnce   sync.Once
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.Reader     { return p.stdout }

func (p *localProcess) Wait() error {
	err := p.cmd.Wait()
	p.exitedOnce.Do(func() { close(p.exited) })
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return &ExitError{Code: exitErr.ExitCode()}
	}
	// nil, exec.ErrWaitDelay (pipes force-closed while a grandchild held
	// them open), or a startup/cancellation error.
	return err
}

// Kill terminates the process and, where the platform allows it, its whole
// process group: background commands spawn children that inherit the output
// pipes, and killing only the leader would leave them running — holding
// ports and the pipe, so Wait would never return. The group first gets
// SIGTERM; a timer escalates to SIGKILL after killGrace, so Kill itself
// never blocks. Killing an already-exited process returns an error the
// caller may ignore.
func (p *localProcess) Kill() error { return p.terminate() }

// Start launches req as a long-running process on the local machine. Like
// Exec, the process is killed when ctx is canceled — with the same
// group-scoped, grace-then-escalate semantics as Kill; callers that need a
// process to outlive a request (servers, background shells) pass
// context.Background() and terminate it with an explicit Kill.
func (l *Local) Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = req.Env
	}
	// A grandchild holding the output pipe must not be able to pin Wait
	// open after the process itself is gone.
	cmd.WaitDelay = pipeWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stdout io.Reader
	if opts.Stdout != nil {
		cmd.Stdout = opts.Stdout
	} else if stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	cmd.Stderr = opts.Stderr // nil connects the child's stderr to the null device
	// The child runs in its own process group so Kill (and context
	// cancellation) can terminate the whole tree, not just the leader.
	newProcessGroup(cmd)
	p := &localProcess{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}
	// Replace exec's default Cancel (a lone Process.Kill) with the
	// group-scoped termination so a canceled context cannot leave
	// grandchildren behind either. Signal errors are swallowed: a group
	// that already exited is expected, and surfacing the error would turn
	// clean exits into Wait failures.
	cmd.Cancel = func() error { _ = p.terminate(); return nil }
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

// ReadFile reads the whole file at path.
func (l *Local) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// WriteFile writes data to path with the given permissions.
func (l *Local) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

// MkdirAll creates path and all missing parents with the given permissions.
func (l *Local) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

// Stat returns file information for path.
func (l *Local) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Stat(path)
}

// Readlink resolves a symbolic link. Local is the only executor that
// implements it: sandboxes do not need the capability — the container is
// the boundary, nothing inside it can point outside (see tools.entryEscapesBase).
func (l *Local) Readlink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return os.Readlink(path)
}

// Glob returns paths matching pattern.
func (l *Local) Glob(ctx context.Context, pattern string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return filepath.Glob(pattern)
}

// Grep walks root, skipping hidden directories (except root itself),
// directories and unreadable files, and returns every line matching pattern.
func (l *Local) Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]GrepMatch, error) {
	var matches []GrepMatch
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entries are skipped.
			if path == root {
				return err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		fileMatches, err := grepFile(path, pattern)
		if err != nil {
			// Unreadable files are skipped.
			return nil
		}
		matches = append(matches, fileMatches...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// grepFile scans a single file line by line for matches.
func grepFile(path string, pattern *regexp.Regexp) ([]GrepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var matches []GrepMatch
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), grepScanBuffer)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if pattern.MatchString(text) {
			matches = append(matches, GrepMatch{Path: path, Line: line, Text: text})
		}
	}
	if err := scanner.Err(); err != nil {
		// Tolerate scan errors (e.g. over-long lines) and keep what matched.
		return matches, nil
	}
	return matches, nil
}
