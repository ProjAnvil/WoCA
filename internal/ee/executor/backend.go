// Package executor defines the EE sandbox execution plane's complete
// dependency on a container runtime: the narrow Backend interface that the
// sandbox services (create/exec/stream endpoints, garbage collection)
// program against, plus an in-process fake implementation for tests.
//
// Nothing in this package imports a Docker client; the production
// implementation lives behind the same interface elsewhere so that plain
// `go test ./...` needs no daemon.
package executor

import (
	"context"
	"io"
	"time"
)

// Hardening selects the security posture a container is created with.
type Hardening int

const (
	// HardeningFull applies every sandbox restriction (read-only root,
	// dropped capabilities, no new privileges). It is the zero value so
	// an unset CreateSpec fails closed.
	HardeningFull Hardening = iota
	// HardeningMinimal relaxes those restrictions for workloads that
	// cannot run fully hardened.
	HardeningMinimal
)

// CreateSpec describes the container a Backend must create.
type CreateSpec struct {
	// Image is the container image reference (required).
	Image string
	// Runtime selects the OCI runtime (e.g. "runsc"); empty means default.
	Runtime string
	// Network is the docker network to attach; empty means default.
	Network string
	// Labels are applied to the container and used by List filters.
	Labels map[string]string
	// PidsLimit caps the container's process count; 0 means runtime default.
	PidsLimit int64
	// Hardening selects the security posture.
	Hardening Hardening
	// WorkDir is the container-side default working directory.
	WorkDir string
}

// ExecSpec describes one command execution inside a container. It mirrors
// the platform ExecRequest semantics (internal/platform/executor): Cmd is
// the full argv with no shell involved, Dir is a container-side path that
// defaults to the container's working directory, Env entries ("K=V")
// overlay the container environment, Stdin is piped to the command, and a
// positive Timeout kills the command (Exec only — Start is driven through
// Process instead).
type ExecSpec struct {
	Cmd     []string
	Dir     string
	Env     []string
	Stdin   []byte
	Timeout time.Duration
}

// ExecResult captures a finished command. As with the platform executor, a
// non-zero exit code is data reported in ExitCode, never an error; an
// error means the command could not run at all or was killed by a
// deadline.
type ExecResult struct {
	// Stdout is the captured standard output.
	Stdout []byte
	// Stderr is the captured standard error.
	Stderr []byte
	// ExitCode is the process exit status; zero on success, -1 when the
	// command could not start or died without a status.
	ExitCode int
}

// OutputEvent is one chunk of captured process output. Start is the
// cumulative byte offset across both streams before this chunk was
// appended; readers poll Snapshot with their last offset and advance it to
// the returned next offset, so events with Start >= offset are exactly the
// ones not yet consumed.
type OutputEvent struct {
	// Stream is "stdout" or "stderr".
	Stream string
	// Data is the captured chunk.
	Data []byte
	// Start is the cumulative output offset before this chunk.
	Start int
}

// State is a container's current status snapshot. The precise liveness
// definition is the backend's: the fake reports Running while a started
// session process is alive, a real runtime reports its container state.
type State struct {
	// Running reports whether the container is executing.
	Running bool
	// ExitCode is the exit status of the most recently finished process
	// (the main process for a real runtime); zero when none finished.
	ExitCode int
	// Labels are the container's labels at creation time.
	Labels map[string]string
}

// Container is one sandbox container driven by a Backend.
type Container interface {
	// ID returns the runtime's container identifier.
	ID() string
	// State reports the container's current status.
	State(ctx context.Context) (State, error)
	// CopyIn extracts tarball (plain tar or gzip-compressed tar) into
	// dstDir, a container-side directory.
	CopyIn(ctx context.Context, dstDir string, tarball io.Reader) error
	// CopyOut packs the container-side path (file or directory tree) into
	// a tar stream; entries are named relative to the path's parent, so
	// the requested path's base name is the top-level entry (Docker
	// archive naming).
	CopyOut(ctx context.Context, path string) (io.ReadCloser, error)
	// Exec runs req to completion. Exit codes pass through: a non-zero
	// exit is reported in ExecResult.ExitCode, not as an error.
	Exec(ctx context.Context, req ExecSpec) (ExecResult, error)
	// Start launches a long-running process and returns a handle to drive
	// it. ExecSpec.Stdin and ExecSpec.Timeout are ignored; feed the
	// process with WriteStdin/CloseStdin and stop it with Kill/Close.
	Start(ctx context.Context, req ExecSpec) (Process, error)
	// Remove deletes the container; force kills its live processes first,
	// while force=false fails while a process is still running.
	Remove(ctx context.Context, force bool) error
}

// Process is a long-running command started by Container.Start.
type Process interface {
	// WriteStdin pipes bytes to the process's standard input.
	WriteStdin(p []byte) error
	// CloseStdin closes the standard input, signalling EOF; idempotent.
	CloseStdin() error
	// Snapshot returns the output events appended at or after offset, the
	// next offset to poll with, whether the process finished, and its
	// exit code once finished.
	Snapshot(offset int) (events []OutputEvent, next int, done bool, exit int)
	// Kill terminates the process; killing an already-exited process is a
	// nil error.
	Kill(ctx context.Context) error
	// Close releases the handle: it closes stdin, kills the process if it
	// is still running (nothing may leak past the caller), waits for
	// output capture to drain, and is idempotent.
	Close() error
}

// Backend is the executor plane's complete dependency on a container
// runtime.
type Backend interface {
	// Create provisions a container per spec.
	Create(ctx context.Context, spec CreateSpec) (Container, error)
	// List returns the containers whose labels match all given pairs, in
	// ID order — the discovery surface for the legacy GC.
	List(ctx context.Context, labels map[string]string) ([]Container, error)
}
