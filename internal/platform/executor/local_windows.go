//go:build windows

package executor

import "os/exec"

// newProcessGroup is a no-op on Windows: process groups are a POSIX
// concept, and the os/exec API offers no group signaling there.
func newProcessGroup(*exec.Cmd) {}

// terminate kills the process itself. A full tree kill on Windows would
// need job objects; until then Kill keeps os/exec's single-process
// semantics (WaitDelay still bounds Wait on pipes held by grandchildren).
func (p *localProcess) terminate() error { return p.cmd.Process.Kill() }
