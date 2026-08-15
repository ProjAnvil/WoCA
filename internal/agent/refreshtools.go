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
// whether the tool list changed.
func (a *Agent) refreshExtraTools(ctx context.Context) bool {
	if a.refreshTools == nil {
		return false
	}
	fresh := a.refreshTools(ctx)
	if fresh == nil {
		fresh = nil
	}
	if sameToolSets(a.extraTools, fresh) {
		return false
	}
	a.extraTools = fresh

	// Rebuild the tool list: base tools (unchanged) + the fresh extras.
	a.toolList = append(append([]coretools.Tool(nil), a.baseTools...), fresh...)
	a.toolByName = make(map[string]coretools.Tool, len(a.toolList))
	for _, t := range a.toolList {
		a.toolByName[t.Name()] = t
	}
	// Rebind the existing model with the new tool set.
	bound := a.model
	if len(a.toolList) > 0 && bound.Capabilities().ToolCalling {
		if b, err := bound.BindTools(a.toolList); err == nil {
			bound = b
		}
	}
	a.mu.Lock()
	a.boundModel = bound
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
