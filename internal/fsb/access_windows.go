//go:build windows

package fsb

import (
	"os"
	"path/filepath"
)

// accessWritable probes writability by creating and removing a throwaway
// file — access(2) semantics are unreliable under Windows ACLs.
func accessWritable(path string) bool {
	probe, err := os.CreateTemp(path, ".woca-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	_ = filepath.SkipAll
	return true
}
