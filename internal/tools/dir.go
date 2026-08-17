package tools

import (
	"context"
	"sync"

	"github.com/projanvil/woca/internal/runws"
)

// Dir is the session's directory state: the base directory the harness
// started in plus any worktrees entered per run workspace. Tools resolve
// relative paths against For(ctx), which prefers a worktree entered for the
// calling run's workspace, then the run workspace itself (serve-mode
// sessions), then the base directory (TUI/REPL runs). This keeps concurrent
// serve sessions over one shared registry isolated from each other's
// worktree switches.
type Dir struct {
	mu        sync.RWMutex
	base      string
	worktrees map[string]string // run base → active worktree
}

// NewDir builds a Dir rooted at dir (empty = ".").
func NewDir(dir string) *Dir {
	if dir == "" {
		dir = "."
	}
	return &Dir{base: dir, worktrees: map[string]string{}}
}

// Base returns the directory the session started in.
func (d *Dir) Base() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.base
}

// BaseFor returns the base directory for the calling run: its workspace when
// the context carries one, else the session's startup directory.
func (d *Dir) BaseFor(ctx context.Context) string {
	if ws, ok := runws.From(ctx); ok && ws != "" {
		return ws
	}
	return d.Base()
}

// For returns the active working directory for the calling run: a worktree
// entered for that run's base, else the base itself.
func (d *Dir) For(ctx context.Context) string {
	base := d.BaseFor(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if wt, ok := d.worktrees[base]; ok {
		return wt
	}
	return base
}

// Enter switches the calling run's base into the given worktree.
func (d *Dir) Enter(ctx context.Context, wt string) {
	if wt == "" {
		return
	}
	base := d.BaseFor(ctx)
	d.mu.Lock()
	d.worktrees[base] = wt
	d.mu.Unlock()
}

// Exit leaves the calling run's worktree, returning it (false when the run
// was not in one).
func (d *Dir) Exit(ctx context.Context) (string, bool) {
	base := d.BaseFor(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	wt, ok := d.worktrees[base]
	if ok {
		delete(d.worktrees, base)
	}
	return wt, ok
}
