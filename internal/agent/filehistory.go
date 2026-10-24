package agent

import (
	"context"
	"fmt"

	"github.com/projanvil/woca/internal/tools"
)

// FileHistoryCounts reports the file-level undo/redo step counts (Write/
// Edit/NotebookEdit mutations; backs /undo and /redo) for the calling run's
// workspace.
func (a *Agent) FileHistoryCounts(ctx context.Context) (undo, redo int) {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return 0, 0
	}
	return a.registry.FileHistory().Counts(ctx)
}

// UndoFiles reverts the most recent file-tool mutation (repeatable) in the
// calling run's workspace.
func (a *Agent) UndoFiles(ctx context.Context) (string, error) {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return "", fmt.Errorf("file history unavailable")
	}
	return a.registry.FileHistory().Undo(ctx)
}

// PendingFiles returns the workspace's pending (undoable) file changes,
// oldest first — the web Changes review tab's payload.
func (a *Agent) PendingFiles(ctx context.Context) []tools.FileChange {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return nil
	}
	return a.registry.FileHistory().Pending(ctx)
}

// UndoFilesSteps reverts up to n file-tool mutations in the calling run's
// workspace (checkpoint rollback), reporting how many were applied.
func (a *Agent) UndoFilesSteps(ctx context.Context, n int) (int, string, error) {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return 0, "", fmt.Errorf("file history unavailable")
	}
	return a.registry.FileHistory().UndoSteps(ctx, n)
}

// UndoFilesThroughPath reverts back to before path was first touched (see
// FileHistory.UndoThroughPath for the unwind semantics).
func (a *Agent) UndoFilesThroughPath(ctx context.Context, path string) (int, string, error) {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return 0, "", fmt.Errorf("file history unavailable")
	}
	return a.registry.FileHistory().UndoThroughPath(ctx, path)
}

// RedoFiles re-applies the most recently undone file mutation in the calling
// run's workspace.
func (a *Agent) RedoFiles(ctx context.Context) (string, error) {
	if a.registry == nil || a.registry.FileHistory() == nil {
		return "", fmt.Errorf("file history unavailable")
	}
	return a.registry.FileHistory().Redo(ctx)
}

// FileHistory exposes the shared undo log (used by tools and tests).
func (a *Agent) FileHistory() *tools.FileHistory {
	if a.registry == nil {
		return nil
	}
	return a.registry.FileHistory()
}

// FileHistoryWatermark returns the pending-change count for the calling
// run's workspace. Snapshot it at run start; FileChangesSince resolves it
// into the run's file mutation delta at retain time.
func (a *Agent) FileHistoryWatermark(ctx context.Context) int {
	return len(a.PendingFiles(ctx))
}

// FileChangesSince returns the workspace's pending changes past the given
// watermark: the run's file mutation delta. An undo during the run shrinks
// the pending list, so the watermark clamps to the current length.
func (a *Agent) FileChangesSince(ctx context.Context, watermark int) []tools.FileChange {
	all := a.PendingFiles(ctx)
	if watermark > len(all) {
		watermark = len(all)
	}
	return all[watermark:]
}

// Close releases the agent's session-scoped background resources: teammates,
// the cron scheduler loop, and pooled LSP servers. Call once when the session
// ends (main's defers); after Close the agent must not start new runs.
func (a *Agent) Close() {
	a.CloseTeams()
	a.stopCron()
	if a.registry != nil {
		a.registry.Close()
	}
}

// CloseTeams stops all teammates spawned by this agent (session shutdown).
func (a *Agent) CloseTeams() {
	if team := a.currentTeam(); team != nil {
		team.Close()
	}
}

// TeamStatusText renders the team status for UIs (empty team message when
// none was spawned).
func (a *Agent) TeamStatusText() (string, error) {
	team := a.currentTeam()
	if team == nil {
		return "no team spawned (the agent creates one with the TeamSpawn tool)\n", nil
	}
	return team.Status(), nil
}
