package agent

import (
	"context"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Between-turn tool refresh, a port of Claude Code's stage 28
// (options.refreshTools): after each tool round the agent re-collects its
// extra tools (e.g. re-listing MCP servers, whose tool set can change at
// runtime) and, when the set changed, rebinds the model so the next round
// sees them. The refresh runs at the end of the tools node, inside the
// graph's sequential node dispatch, so no node observes a half-updated set.

// refreshExtraTools re-collects the extra (non-registry) tools via the
// configured hook and rebinds the model when the set changed. It reports
// whether the tool list changed. The collection hook and the model rebind run
// outside a.mu; the compare-and-commit of the tool fields is serialized by
// a.mu so the run-side readers (execute, route, ToolNames) never observe a
// half-updated set.
func (a *Agent) refreshExtraTools(ctx context.Context) bool {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refreshTools == nil {
		return false
	}
	fresh := a.refreshTools(ctx)
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

// RefreshToolsNow re-collects the extra tools outside a run (e.g. the
// settings API toggled a plugin and the new tool set should be bound before
// the next turn starts). Safe to call while a run is in flight: refreshes are
// serialized with each other via refreshMu and the run-side readers take
// their snapshots under a.mu.
func (a *Agent) RefreshToolsNow(ctx context.Context) {
	a.refreshExtraTools(ctx)
}
