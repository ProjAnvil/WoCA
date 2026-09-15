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
)

// grepScanBuffer is the maximum line size Grep can scan.
const grepScanBuffer = 1 << 20 // 1 MiB

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
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.Reader     { return p.stdout }

func (p *localProcess) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return &ExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

func (p *localProcess) Kill() error { return p.cmd.Process.Kill() }

// Start launches req as a long-running process on the local machine. Like
// Exec, the process is killed when ctx is canceled; callers that need a
// process to outlive a request (servers, background shells) pass
// context.Background() and terminate it with an explicit Kill.
func (l *Local) Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = req.Env
	}
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
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &localProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
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
