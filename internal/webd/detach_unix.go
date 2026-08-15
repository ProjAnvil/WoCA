//go:build unix

package webd

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session/process group so it survives the
// CLI exiting and Ctrl-C in the launching terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
