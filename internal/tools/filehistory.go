package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/projanvil/woca/internal/runws"
)

// FileHistory is the file-level undo/redo log backing /undo and /redo (woca's
// counterpart of opencode's /undo). Every
// mutation performed through the Write/Edit/NotebookEdit tools records the
// file's before/after bytes; undo restores the earlier state (deleting files
// the change created), redo re-applies it. Consecutive changes to the same
// path coalesce into one undo step so a turn that writes then tweaks one file
// reverts with a single /undo.
//
// Stacks are scoped per run workspace (see internal/runws): concurrent
// serve-mode sessions undo only their own session's edits. The undecorated
// default scope covers TUI/REPL runs.
//
// Bash-caused filesystem changes are NOT tracked (matching opencode's
// edits-only scope); git remains the recovery path there.
type FileHistory struct {
	mu     sync.Mutex
	scopes map[string]*fileUndoStack
}

// fileUndoStack is one workspace's undo/redo stacks.
type fileUndoStack struct {
	undo []FileChange
	redo []FileChange
}

// FileChange is one reversible file mutation. A nil Before means the file did
// not exist before the change; nil After means it was deleted.
type FileChange struct {
	Path   string
	Before []byte
	After  []byte
}

// NewFileHistory builds an empty history.
func NewFileHistory() *FileHistory {
	return &FileHistory{scopes: map[string]*fileUndoStack{}}
}

// scope resolves (under lock) the stack for the calling run's workspace.
func (h *FileHistory) scope(ctx context.Context) *fileUndoStack {
	ws, _ := runws.From(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.scopes[ws]
	if s == nil {
		s = &fileUndoStack{}
		h.scopes[ws] = s
	}
	return s
}

// Record logs a completed mutation and clears the redo stack. after may be
// nil for deletions.
func (h *FileHistory) Record(ctx context.Context, path string, before, after []byte) {
	s := h.scope(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	change := FileChange{Path: path, Before: before, After: after}
	// Coalesce consecutive changes to the same path: keep the earliest
	// Before, adopt the latest After.
	if n := len(s.undo); n > 0 && s.undo[n-1].Path == path {
		s.undo[n-1].After = after
	} else {
		s.undo = append(s.undo, change)
	}
	s.redo = nil
}

// Undo restores the most recent change and returns a human-readable
// description. The stacks only move after the restore hits the disk: a
// failed apply leaves the step undoable instead of losing it between stacks.
func (h *FileHistory) Undo(ctx context.Context) (string, error) {
	s := h.scope(ctx)
	h.mu.Lock()
	if len(s.undo) == 0 {
		h.mu.Unlock()
		return "nothing to undo", nil
	}
	change := s.undo[len(s.undo)-1]
	h.mu.Unlock()

	if err := applyState(change.Path, change.Before); err != nil {
		return "", err
	}
	h.mu.Lock()
	s.undo = s.undo[:len(s.undo)-1]
	s.redo = append(s.redo, change)
	h.mu.Unlock()
	return describeRestore("Undid", change, change.Before), nil
}

// Redo re-applies the most recently undone change (same apply-then-commit
// ordering as Undo).
func (h *FileHistory) Redo(ctx context.Context) (string, error) {
	s := h.scope(ctx)
	h.mu.Lock()
	if len(s.redo) == 0 {
		h.mu.Unlock()
		return "nothing to redo", nil
	}
	change := s.redo[len(s.redo)-1]
	h.mu.Unlock()

	if err := applyState(change.Path, change.After); err != nil {
		return "", err
	}
	h.mu.Lock()
	s.redo = s.redo[:len(s.redo)-1]
	s.undo = append(s.undo, change)
	h.mu.Unlock()
	return describeRestore("Redid", change, change.After), nil
}

// Pending returns a copy of the workspace's undoable changes, oldest
// first — the web Changes review tab's payload (un-reverted edits with
// their before/after bytes).
func (h *FileHistory) Pending(ctx context.Context) []FileChange {
	s := h.scope(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]FileChange, len(s.undo))
	copy(out, s.undo)
	return out
}

// UndoSteps reverts up to n changes from the top of the stack, reporting how
// many were applied (0 when there was nothing to undo). Each step keeps
// Undo's apply-then-commit ordering, so a failed apply stops the batch with
// the earlier steps already reverted and reported.
func (h *FileHistory) UndoSteps(ctx context.Context, n int) (int, string, error) {
	applied := 0
	var descs []string
	for i := 0; i < n; i++ {
		desc, err := h.Undo(ctx)
		if err != nil {
			return applied, "", err
		}
		if desc == "nothing to undo" {
			break
		}
		applied++
		descs = append(descs, desc)
	}
	return applied, strings.Join(descs, "; "), nil
}

// UndoThroughPath reverts changes from the top of the stack down to (and
// including) the oldest change touching path. The stack only unwinds from
// the top, so changes to other files made after that point revert with it —
// the semantics of "roll back to before this file was first touched".
func (h *FileHistory) UndoThroughPath(ctx context.Context, path string) (int, string, error) {
	s := h.scope(ctx)
	h.mu.Lock()
	target := -1
	for i, ch := range s.undo {
		if ch.Path == path {
			target = i
			break
		}
	}
	n := 0
	if target >= 0 {
		n = len(s.undo) - target
	}
	h.mu.Unlock()
	if target < 0 {
		return 0, "no changes for " + path, nil
	}
	return h.UndoSteps(ctx, n)
}

// Counts reports the undoable and redoable step counts (for UI hints).
func (h *FileHistory) Counts(ctx context.Context) (undo, redo int) {
	s := h.scope(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(s.undo), len(s.redo)
}

// applyState writes state to path (nil removes the file), preserving the
// current file mode when the file exists.
func applyState(path string, state []byte) error {
	if state == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		mode = fi.Mode()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	if err := os.WriteFile(path, state, mode); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	return nil
}

func describeRestore(verb string, change FileChange, state []byte) string {
	rel := change.Path
	if state == nil {
		return fmt.Sprintf("%s change to %s (file removed)", verb, rel)
	}
	if change.Before == nil {
		return fmt.Sprintf("%s creation of %s (%d bytes restored away)", verb, rel, len(state))
	}
	return fmt.Sprintf("%s change to %s (%d bytes restored)", verb, rel, len(state))
}
