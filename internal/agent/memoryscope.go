package agent

import (
	"context"
	"sync"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/store"
)

// memoryScope carries one run's memory prefetch slot (runws-style ctx
// decoration): concurrent runs on a shared agent no longer overwrite each
// other's Begin results. Contexts without a scope (CLI single-run paths)
// fall back to the agent-level memoryRuntime.prefetch field.
type memoryScope struct {
	mu         sync.Mutex
	entries    []store.Entry
	engineHits []memory.Hit
}

type memoryScopeKey struct{}

// WithMemoryScope decorates ctx with a fresh per-run scope. runner.Run and
// runner.RunGoal call this once at run entry.
func WithMemoryScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoryScopeKey{}, &memoryScope{})
}

func memoryScopeFrom(ctx context.Context) *memoryScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(memoryScopeKey{}).(*memoryScope)
	return s
}
