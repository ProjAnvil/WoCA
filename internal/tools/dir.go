package tools

import "sync"

// Dir is the session's mutable working directory. Tools resolve relative
// paths against Current(), which EnterWorktree/ExitWorktree swap when the
// agent moves into an isolated git worktree; Base() stays the original
// directory the session started in.
type Dir struct {
	mu   sync.RWMutex
	dir  string
	base string
}

// NewDir builds a Dir rooted at dir (empty = ".").
func NewDir(dir string) *Dir {
	if dir == "" {
		dir = "."
	}
	return &Dir{dir: dir, base: dir}
}

// Current returns the active working directory.
func (d *Dir) Current() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.dir
}

// Base returns the directory the session started in.
func (d *Dir) Base() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.base
}

// Set switches the active working directory.
func (d *Dir) Set(dir string) {
	if dir == "" {
		return
	}
	d.mu.Lock()
	d.dir = dir
	d.mu.Unlock()
}
