//go:build unix

package executor

import (
	"fmt"
	"syscall"
)

// statfsUsage reports the filesystem usage of path's containing
// filesystem in percent (blocks used over blocks total).
func statfsUsage(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Blocks <= 0 {
		return 0, nil
	}
	used := st.Blocks - st.Bfree
	return float64(used) * 100 / float64(st.Blocks), nil
}
