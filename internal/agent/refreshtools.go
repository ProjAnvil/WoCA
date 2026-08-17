package agent

import (
	"context"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Between-turn tool refresh, a port of Claude Code's stage 28
// (options.refreshTools): after each tool round the agent re-collects its
// extra tools (e.g. re-listing MCP servers, whose tool set can change at
// runtime) and, when the set changed, rebinds the model so the next round
// sees them.

// refreshTTL throttles between-turn extra-tool refreshes: within the window
// the tools node skips re-collection entirely.
var refreshTTL = 5 * time.Minute

// maybeRefreshTools triggers a background extra-tool refresh when the TTL
// has expired. The current round keeps the existing bindings — a slow MCP
// re-list must never block the tools node (previously refreshMu covered the
// network I/O and stalled every run each round).
func (a *Agent) maybeRefreshTools(ctx context.Context) {
	if a.refreshTools == nil {
		return
	}
	if time.Since(time.Unix(0, a.refreshLast.Load())) < refreshTTL {
		return
	}
	// DoChan runs fn in its own goroutine; duplicate calls while one is in
	// flight share the result (singleflight).
	res := a.refreshSF.DoChan("extra-tools", func() (any, error) {
		// Outlive the tool round's ctx so an end-of-run cancel doesn't abort
		// a half-done re-list.
		a.refreshExtraTools(context.WithoutCancel(ctx))
		return nil, nil
	})
	go func() {
		<-res
		a.refreshLast.Store(time.Now().UnixNano())
	}()
}

// refreshExtraTools re-collects the extra (non-registry) tools via the
// configured hook and rebinds the model when the set changed. Collection
// and rebind run WITHOUT refreshMu (network I/O must not hold it); refreshMu
// now only serializes the microsecond compare-and-commit against
// RefreshToolsNow-driven swaps. Run-side readers take snapshots under a.mu.
func (a *Agent) refreshExtraTools(ctx context.Context) bool {
	if a.refreshTools == nil {
		return false
	}
	fresh := a.refreshTools(ctx)
	a.refreshMu.Lock()
	a.mu.Lock()
	changed := !sameToolSets(a.extraTools, fresh)
	if changed {
		a.extraTools = fresh
		// Rebuild the tool list: base tools (unchanged) + the fresh extras.
		a.toolList = append(append([]coretools.Tool(nil), a.baseTools...), fresh...)
		a.toolByName = make(map[string]coretools.Tool, len(a.toolList))
		for _, t := range a.toolList {
			a.toolByName[t.Name()] = t
		}
	}
	list := a.toolList
	model := a.model
	a.mu.Unlock()
	a.refreshMu.Unlock()
	if !changed {
		return false
	}
	// Rebind the existing model with the new tool set (the base tools are
	// already folded into list).
	bound := model
	if len(list) > 0 && bound.Capabilities().ToolCalling {
		if b, err := bound.BindTools(list); err == nil {
			bound = b
		}
	}
	a.mu.Lock()
	// Skip the commit if SetModel swapped the model meanwhile: its binding
	// (taken against the already-committed tool list) wins, and the next
	// refresh re-rebinds.
	if a.model == model {
		a.boundModel = bound
	}
	a.mu.Unlock()
	return true
}

// sameToolSets compares two tool sets by name.
func sameToolSets(a, b []coretools.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	names := make(map[string]int, len(a))
	for _, t := range a {
		names[t.Name()]++
	}
	for _, t := range b {
		names[t.Name()]--
		if names[t.Name()] < 0 {
			return false
		}
	}
	return true
}

// RefreshToolsNow re-collects immediately, bypassing the TTL; concurrent
// callers (and an in-flight background refresh) are deduplicated by the
// same singleflight key.
func (a *Agent) RefreshToolsNow(ctx context.Context) {
	_, _, _ = a.refreshSF.Do("extra-tools", func() (any, error) {
		a.refreshExtraTools(ctx)
		a.refreshLast.Store(time.Now().UnixNano())
		return nil, nil
	})
}
