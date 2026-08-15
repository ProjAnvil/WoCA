//go:build windows

package webd

import "os/exec"

// detach is a no-op on Windows: child processes are independent of the
// parent console by default.
func detach(cmd *exec.Cmd) {}
