// Package runexec carries the run-scoped executor through the context
// (design §10.1 C-3, the WithRunExecutor decision): a per-run sandbox
// overrides the build-time executor without rebuilding the agent. One
// decoration scopes a whole run — the tool closures, hooks, background
// shells and gate checks all resolve it identically via RunExecutorFrom,
// falling back to their build-time instances when absent. It lives
// outside internal/agent and internal/tools so both (and the mcp/hooks
// packages) can import it without cycles.
package runexec

import (
	"context"

	"github.com/projanvil/woca/internal/platform/executor"
)

type key struct{}

// WithRunExecutor returns a context carrying the run's executor. A nil
// executor is a no-op (the fallbacks apply).
func WithRunExecutor(ctx context.Context, x executor.Executor) context.Context {
	if x == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, x)
}

// RunExecutorFrom reports the run-scoped executor, if any.
func RunExecutorFrom(ctx context.Context) (executor.Executor, bool) {
	if ctx == nil {
		return nil, false
	}
	x, ok := ctx.Value(key{}).(executor.Executor)
	return x, ok
}
