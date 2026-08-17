package merge

import (
	"os"
	"path/filepath"
	"sync"
)

// workspaceCache memoizes WorkspaceDirs per workDir: serve mode resolves a
// chain per session workspace and rebuilds prompts often, so the walk (and
// its stats) must not repeat. Entries never expire within the process
// lifetime — a workspace that runs `git init` later keeps its old chain
// until restart (accepted per design §8 "启动时算一次并缓存").
var workspaceCache sync.Map // workDir -> []string

// WorkspaceDirs returns the workspace directory chain for workDir: workDir
// first, then each ancestor up to and including the git root. A .git
// directory or the file git worktrees use both mark the root; outside a
// repository the chain is workDir alone (D3).
func WorkspaceDirs(workDir string) []string {
	if v, ok := workspaceCache.Load(workDir); ok {
		return append([]string(nil), v.([]string)...)
	}
	chain := workspaceDirs(workDir)
	workspaceCache.Store(workDir, chain)
	return append([]string(nil), chain...)
}

// ResetWorkspaceCache drops cached WorkspaceDirs results (tests).
func ResetWorkspaceCache() {
	workspaceCache = sync.Map{}
}

func workspaceDirs(workDir string) []string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	root := findGitRoot(abs)
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if root == "" || dir == root || dir == filepath.Dir(dir) {
			break
		}
	}
	return chain
}

// findGitRoot walks up from dir looking for .git (directory, or the file
// git worktrees use). It returns "" when no repository is found.
func findGitRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
