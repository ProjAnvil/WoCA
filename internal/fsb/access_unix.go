//go:build unix

package fsb

import "syscall"

// accessWritable probes real write permission (ACLs included) with
// access(2); W_OK is 2 on darwin/linux.
func accessWritable(path string) bool {
	return syscall.Access(path, 2) == nil
}
