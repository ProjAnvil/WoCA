package agent

import "context"

// memoryWorkspaceKey scopes the run's memory workspace through the context
// (values survive context.WithoutCancel and cancellation itself).
type memoryWorkspaceKey struct{}

// WithMemoryWorkspace returns a context carrying the run's memory workspace.
// The web server wraps each run's context with the session workspace;
// TUI/REPL runs don't need it (agent field/workdir fallback applies).
func WithMemoryWorkspace(ctx context.Context, ws string) context.Context {
	if ws == "" {
		return ctx
	}
	return context.WithValue(ctx, memoryWorkspaceKey{}, ws)
}

// memoryWorkspaceFromContext reports the run-scoped workspace, if any.
func memoryWorkspaceFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	ws, ok := ctx.Value(memoryWorkspaceKey{}).(string)
	return ws, ok
}

// memoryWorkspaceCtx resolves the memory workspace for one run: the context
// value first (serve mode: the session workspace, race-free under concurrent
// runs), then the agent field (TUI/REPL startup pin, subagent fork
// inheritance), then the workdir.
func (a *Agent) memoryWorkspaceCtx(ctx context.Context) string {
	if ws, ok := memoryWorkspaceFromContext(ctx); ok {
		return ws
	}
	return a.memoryWorkspace()
}
