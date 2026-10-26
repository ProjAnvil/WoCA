//go:build unix

package executor

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

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
