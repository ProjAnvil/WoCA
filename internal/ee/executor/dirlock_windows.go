//go:build windows

package executor

import (
	"errors"
)

// errLockUnsupported is the windows posture for workspace locking: the
// executor's locking (like its runtime contract — the docker socket) is
// a unix-host facility, so the build refuses loudly instead of silently
// degrading to no locking. The executor role is deployed on Linux hosts
// only; a windows binary of this package exists so the tree cross-builds
// and vet-checks, not as a supported target.
var errLockUnsupported = errors.New("workspace locking requires a unix host; the executor is not supported on windows")

// LockDir refuses: workspace locking has no windows implementation. The
// error is explicit and permanent — callers fail closed (a workspace
// cannot be routed on a host that cannot serialize its access).
func LockDir(path string) (*DirLock, error) {
	return nil, errLockUnsupported
}

// Close is a no-op: no lock can ever be held (LockDir always refuses),
// and a nil receiver is tolerated like the unix side.
func (l *DirLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close() // unreachable via LockDir; defensive only
}
