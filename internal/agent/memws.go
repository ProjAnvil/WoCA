package agent

import (
	"context"

	"github.com/projanvil/woca/internal/runws"
)

// WithRunWorkspace returns a context carrying the run's workspace. The web
// server wraps each run's context with the session workspace, which scopes
// the run's memory calls, system prompt environment and file/shell tools at
// once (see internal/runws); TUI/REPL runs don't need it (the agent
// field/workdir fallbacks apply).
func WithRunWorkspace(ctx context.Context, ws string) context.Context {
	return runws.With(ctx, ws)
}

// RunWorkspaceFromContext reports the run-scoped workspace, if any.
func RunWorkspaceFromContext(ctx context.Context) (string, bool) {
	return runws.From(ctx)
}

// memoryWorkspaceCtx resolves the memory workspace for one run: the context
// value first (serve mode: the session workspace, race-free under concurrent
// runs), then the agent field (TUI/REPL startup pin, subagent fork
// inheritance), then the workdir.
func (a *Agent) memoryWorkspaceCtx(ctx context.Context) string {
	if ws, ok := runws.From(ctx); ok {
		return ws
	}
	return a.memoryWorkspace()
}
