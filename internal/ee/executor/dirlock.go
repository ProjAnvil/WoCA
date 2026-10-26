package executor

import (
	"errors"
	"os"
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
//
// The locking primitives are platform-split: dirlock_unix.go carries the
// flock implementation; dirlock_windows.go refuses (the executor is a
// Linux-host role — the refusal is loud, never a silent no-lock).
type DirLock struct {
	path string
	f    *os.File
}

// Path returns the locked directory's path.
func (l *DirLock) Path() string { return l.path }
