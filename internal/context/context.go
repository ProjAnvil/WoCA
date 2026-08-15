// Package contextx collects runtime context (git status, etc.) that is
// injected into the system prompt, mirroring Claude Code's context.ts.
package contextx

import (
	"os/exec"
	"strings"
)

// Collect builds the context block for a working directory.
func Collect(workdir string) string {
	var b strings.Builder
	if status, ok := GitStatus(workdir); ok && strings.TrimSpace(status) != "" {
		b.WriteString("<git_status>\n")
		b.WriteString(status)
		if !strings.HasSuffix(status, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("</git_status>\n")
	}
	return b.String()
}

// GitStatus returns the short git status (with branch) for dir, and whether
// dir is a git repository.
func GitStatus(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "status", "--short", "--branch").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}
