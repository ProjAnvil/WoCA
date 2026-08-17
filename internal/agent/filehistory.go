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

// Close releases the agent's session-scoped background resources: teammates
// and the cron scheduler loop. Call once when the session ends (main's
// defers); after Close the agent must not start new runs.
func (a *Agent) Close() {
	a.CloseTeams()
	a.stopCron()
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
