package prompt

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/projanvil/woca/internal/merge"
	"github.com/projanvil/woca/internal/tokens"
)

// memoryFile is one instruction file to inject into the system prompt.
type memoryFile struct {
	Path  string // absolute path on disk
	Kind  string // header kind: "User" or "Project"
	Label string // header label, e.g. "CLAUDE.md" or ".claude/CLAUDE.md"
}

// collectMemoryFiles gathers layered instruction files (D4): the user-level
// ~/.woca/CLAUDE.md first (falling back to ~/.claude/CLAUDE.md), then every
// CLAUDE.md / AGENTS.md / .claude/CLAUDE.md found walking from the git root
// down to workDir — the more local a file, the later it is injected.
func collectMemoryFiles(workDir string) []memoryFile {
	var out []memoryFile
	// seen deduplicates by absolute path: when the git root == $HOME,
	// ~/.claude/CLAUDE.md would match both the user-level fallback and the
	// project-level walk; inject it only once.
	seen := map[string]bool{}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			filepath.Join(home, ".woca", "CLAUDE.md"),
			filepath.Join(home, ".claude", "CLAUDE.md"),
		} {
			if fileExists(p) {
				seen[p] = true
				out = append(out, memoryFile{Path: p, Kind: "User", Label: "CLAUDE.md"})
				break
			}
		}
	}
	dirs := merge.WorkspaceDirs(workDir)
	for _, dir := range slices.Backward(dirs) { // git root first, workDir last
		for _, name := range []string{"CLAUDE.md", "AGENTS.md", ".claude/CLAUDE.md"} {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if fileExists(p) && !seen[p] {
				seen[p] = true
				out = append(out, memoryFile{Path: p, Kind: "Project", Label: name})
			}
		}
	}
	return out
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// MemoryFileStats is the token footprint of a workspace's memory files: the
// memory-files slice of the system-prompt breakdown shown by /context.
type MemoryFileStats struct {
	TotalTokens int
	Files       int
}

// MemoryFileStatsFor estimates the token size of the instruction files
// injected into the system prompt for workDir (heuristic estimator, same one
// the compaction threshold uses).
func MemoryFileStatsFor(workDir string) MemoryFileStats {
	var out MemoryFileStats
	for _, mf := range collectMemoryFiles(workDir) {
		b, err := os.ReadFile(mf.Path)
		if err != nil {
			continue
		}
		out.TotalTokens += tokens.EstimateText(string(b))
		out.Files++
	}
	return out
}
