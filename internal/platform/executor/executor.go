// Package executor defines the tool-execution boundary for the platform
// layer. Business code depends only on the Executor interface; the local
// implementation runs commands and file operations on the current machine.
package executor

import (
	"context"
	"io"
	"io/fs"
	"regexp"
	"strconv"
)

// ExecRequest describes a single command execution.
type ExecRequest struct {
	// Name is the command to run, resolved via PATH when not a path.
	Name string
	// Args are the command arguments.
	Args []string
	// Dir is the working directory; empty means the current directory.
	Dir string
	// Env overrides the environment for the command; nil inherits os.Environ.
	Env []string
	// Stdin is piped to the command's standard input.
	Stdin []byte
}

// ExecResult captures the outcome of a finished command.
type ExecResult struct {
	// Stdout is the captured standard output.
	Stdout []byte
	// Stderr is the captured standard error.
	Stderr []byte
	// ExitCode is the process exit code; zero on success.
	ExitCode int
}

// GrepMatch is a single line matching a grep pattern.
type GrepMatch struct {
	// Path is the file containing the match.
	Path string
	// Line is the 1-based line number of the match.
	Line int
	// Text is the full text of the matching line.
	Text string
}

// Executor abstracts command execution and filesystem access so business
// code stays independent of where the operations run.
type Executor interface {
	// Exec runs a command. A non-zero exit code is reported via
	// ExecResult.ExitCode, not as an error; an error is returned only when
	// the process cannot be started or the context is canceled.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// Start launches a long-running command (RPC protocols, servers,
	// background shells) and returns a handle to drive it. The process is
	// killed when ctx is canceled — callers that need it to outlive a
	// request pass context.Background() and Kill explicitly. A non-zero
	// exit surfaces from Process.Wait as *ExitError, never from Start
	// itself; ExecRequest.Stdin is ignored — write to Process.Stdin
	// instead.
	Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error)
	// ReadFile reads the whole file at path.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes data to path with the given permissions.
	WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
	// MkdirAll creates path and all missing parents with the given permissions.
	MkdirAll(ctx context.Context, path string, perm fs.FileMode) error
	// Stat returns file information for path.
	Stat(ctx context.Context, path string) (fs.FileInfo, error)
	// Glob returns paths matching pattern.
	Glob(ctx context.Context, pattern string) ([]string, error)
	// Grep walks root and returns lines matching pattern. Hidden
	// directories (names starting with ".") are skipped, except root itself.
	Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]GrepMatch, error)
}

// StartOptions selects where a started process's output goes.
type StartOptions struct {
	// Stdout, when non-nil, receives the process's standard output and
	// Process.Stdout returns nil. When nil, output is readable from the
	// pipe returned by Process.Stdout — the caller must read it or the
	// process may block once the pipe buffer fills.
	Stdout io.Writer
	// Stderr receives the process's standard error; nil discards it (the
	// child's stderr is connected to the null device). Stderr is
	// diagnostics only, so unlike stdout it is never exposed as a pipe.
	Stderr io.Writer
}

// Process is a long-running command started by Executor.Start.
type Process interface {
	// Stdin is the process's standard input pipe; closing it signals EOF.
	Stdin() io.WriteCloser
	// Stdout is the process's standard output pipe, or nil when
	// StartOptions.Stdout redirected it.
	Stdout() io.Reader
	// Wait blocks until the process exits: nil on exit status 0,
	// *ExitError on a non-zero status. Wait must not run concurrently from
	// multiple goroutines. Redirected output writes finish before Wait returns.
	Wait() error
	// Kill terminates the process. Killing an already-exited process
	// returns an error the caller may ignore.
	Kill() error
}

// ExitError reports a process that exited with a non-zero status.
type ExitError struct {
	// Code is the process exit status.
	Code int
}

// Error matches os/exec's wording so error surfaces stay unchanged.
func (e *ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }
