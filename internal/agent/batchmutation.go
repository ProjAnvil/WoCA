package agent

import (
	"path/filepath"

	"github.com/projanvil/langchain-golang/core/messages"
)

// fileMutationKey returns the same-batch serialization key for a call that
// mutates exactly one file: the cleaned path for the core file writers.
// Every other call — reads, shell, meta tools — returns "" (no key, no
// serialization). Shell is excluded because its file effects are not
// statically knowable; the write plane is where one batch can carry two
// mutations of the SAME path, which must apply in call order instead of
// racing (see toolsNode and batchmutation_test.go).
func fileMutationKey(call messages.ToolCall) string {
	var raw string
	switch call.Name {
	case "Write", "Edit":
		raw, _ = call.Args["file_path"].(string)
	case "NotebookEdit":
		raw, _ = call.Args["notebook_path"].(string)
	default:
		return ""
	}
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}
