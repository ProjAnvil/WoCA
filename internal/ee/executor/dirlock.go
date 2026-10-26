package executor

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrWorkspaceBusy reports that a workspace directory's lock is already
// held (by another request or process); the caller should not wait on it.
var ErrWorkspaceBusy = errors.New("executor: workspace directory busy")

// DirLock is an exclusive advisory lock on a workspace directory, taken
// with flock(2) on the directory's own file descriptor. Two properties
// drove this design:
//
//   - The lock lives on the directory fd, not on a lock file inside the
//     directory, so locking never pollutes the tree that gets tarred for
//     upload.
//   - The kernel releases the lock when the process dies in any way
//     (crash, kill -9), so a dead holder can never wedge a workspace.
//
// Holders must serialize Upload/Sync/Delete of a workspace by holding its
// DirLock for the whole critical section. Close releases the lock and is
// idempotent.
type DirLock struct {
	path string
	f    *os.File
}

// LockDir acquires an exclusive, non-blocking flock on the directory at
// path. If the lock is already held — including by the same process on a
// different descriptor — it returns an error wrapping ErrWorkspaceBusy.
// EINTR from the flock call is retried.
func LockDir(path string) (*DirLock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("workspace lock: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workspace lock: stat %s: %w", path, err)
	}
	if !info.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("workspace lock: %s is not a directory", path)
	}

	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("workspace lock: %s: %w", path, ErrWorkspaceBusy)
		}
		return nil, fmt.Errorf("workspace lock: flock %s: %w", path, err)
	}
	return &DirLock{path: path, f: f}, nil
}

// Path returns the locked directory's path.
func (l *DirLock) Path() string { return l.path }

// Close releases the lock and closes the directory descriptor. Closing
// the descriptor would release the lock anyway; the explicit unlock keeps
// the release timely and diagnosable. Idempotent.
func (l *DirLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("workspace lock: release %s: %w", l.path, err)
	}
	return nil
}
