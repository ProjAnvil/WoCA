// Package runws carries the run-scoped workspace through the context: the
// directory a serve-mode run's session belongs to. One decoration scopes a
// whole run — the system prompt environment, the file/shell tools, and the
// memory subsystem all resolve it identically (ctx value first, then the
// harness's startup/workdir fallbacks), so concurrent runs over a shared
// agent stay isolated without agent-wide mutable state.
package runws

import "context"

type key struct{}

// With returns a context carrying the run's workspace. An empty workspace is
// a no-op (the fallbacks apply).
func With(ctx context.Context, ws string) context.Context {
	if ws == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, ws)
}

// From reports the run-scoped workspace, if any.
func From(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	ws, ok := ctx.Value(key{}).(string)
	return ws, ok
}
