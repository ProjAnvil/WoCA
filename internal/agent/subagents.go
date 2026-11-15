package agent

// Content-addressed subagent child registry (graph-native HITL Task 6,
// design §3.2). Every custom subagent definition visible to the agent —
// the per-workspace wsAgents cache plus the startup set — is built into a
// child *Agent once and registered in the parent graph as a subgraph node
// named `subagent:<sanitized-def-name>-<fp4>`, where fp4 is a 4-hex-char
// FNV-1a fingerprint of the def's content. Content addressing makes
// cross-workspace same-name defs with different content distinct nodes and
// makes def edits change the node name; identical content dedupes to one
// node. A def-set change (a wsAgents Set — LRU eviction rides one — or a
// SetAgentDefinitions swap) schedules a debounced parent recompile that
// swaps a.graph under a.mu; runs in flight keep the compiled-graph
// instance they captured at their entry point.
//
// Task 7 routes foreground Task calls into these nodes via types.Send from
// route2; this file only builds and registers them.

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/runws"
)

// subagentNodePrefix marks parent-graph nodes backed by subagent child
// graphs. Persisted in checkpoints (planned task node names), so stable
// across releases.
const subagentNodePrefix = "subagent:"

// graphRebuildDebounce collapses rapid successive def-set changes (a run
// resolving several workspaces, an /agents manager save) into one parent
// recompile.
const graphRebuildDebounce = 250 * time.Millisecond

// defFingerprint returns the 4-hex-char content fingerprint of a subagent
// definition: FNV-1a over the name, the prompt and the sorted tool list.
// agents.Definition carries no model of its own (children run on the
// parent's subagent model), so no model participates. The hash is pure —
// stable across rebuilds and across processes.
func defFingerprint(def agents.Definition) string {
	h := fnv.New32a()
	fields := append([]string{def.Name, def.Prompt}, def.Tools...)
	slices.Sort(fields[2:]) // tool order must not matter
	for _, f := range fields {
		_, _ = io.WriteString(h, f)
		_, _ = h.Write([]byte{0}) // field separator: no prefix ambiguity
	}
	return fmt.Sprintf("%04x", h.Sum32()&0xffff)
}

// sanitizeNodeToken maps an arbitrary def name into the node-name alphabet:
// [a-zA-Z0-9-_] survives, every other rune collapses to '-'.
func sanitizeNodeToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// subagentNodeName returns the parent-graph node name for one definition:
// "subagent:<sanitized-name>-<fp4>". Identical content maps to one node;
// any content edit mints a new one.
func subagentNodeName(def agents.Definition) string {
	return subagentNodePrefix + sanitizeNodeToken(def.Name) + "-" + defFingerprint(def)
}

// subagentDefLabel best-effort recovers the def-name segment a subagent
// node name was built from (resume errors use it to name the definition).
func subagentDefLabel(node string) string {
	s := strings.TrimPrefix(node, subagentNodePrefix)
	if i := strings.LastIndex(s, "-"); i >= 0 {
		s = s[:i]
	}
	return s
}

// childRegistry is the content-addressed set of built subagent children:
// subagent node name -> child agent (built via the newSubagent path).
// Rebuilt whole by rebuildGraph; the pointer is published under a.mu and
// the methods are safe for concurrent use.
type childRegistry struct {
	mu       sync.RWMutex
	children map[string]*Agent
}

func newChildRegistry() *childRegistry {
	return &childRegistry{children: map[string]*Agent{}}
}

func (r *childRegistry) set(node string, child *Agent) {
	r.mu.Lock()
	r.children[node] = child
	r.mu.Unlock()
}

func (r *childRegistry) lookup(node string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.children[node]
}

// nodes returns the sorted registered node names (a snapshot).
func (r *childRegistry) nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.children))
}

func (r *childRegistry) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.children)
}

// defSite is one (workspace, definition) pair of the registration union.
type defSite struct {
	ws  string
	def agents.Definition
}

// defSites collects the registration union: every definition currently
// cached across workspaces (serve mode resolves per-workspace defs through
// AgentsFor into the wsAgents cache) plus the startup set (TUI/CLI/tests
// resolve through a.agentDefs when no loader is wired — leaving it out
// would leave those deployments without subagent nodes). Sorted
// deterministically so identical-content dedup picks a stable winner.
func (a *Agent) defSites() []defSite {
	var sites []defSite
	for ws, defs := range a.wsAgents.snapshot() {
		for _, d := range defs {
			sites = append(sites, defSite{ws: ws, def: d})
		}
	}
	a.mu.Lock()
	startup := a.agentDefs
	a.mu.Unlock()
	home := a.memoryWorkspace()
	for _, d := range startup {
		sites = append(sites, defSite{ws: home, def: d})
	}
	slices.SortFunc(sites, func(x, y defSite) int {
		return cmp.Or(
			cmp.Compare(x.def.Name, y.def.Name),
			cmp.Compare(x.ws, y.ws),
			cmp.Compare(x.def.Prompt, y.def.Prompt),
		)
	})
	return sites
}

// buildChildRegistry builds one child agent per distinct def content via
// the newSubagent path (live Permissions/Hooks/HooksFor/ToolGuards and the
// parent's saver flow into every child; see newSubagent).
//
// A def whose child fails to build is SKIPPED, not fatal: New must not die
// because one workspace's def is unloadable. The failure still surfaces at
// use time exactly the way newSubagent errors surface today — the Task
// tool builds its subagent through the same newSubagent and reports the
// error as the call's tool result. The skip is logged to the
// observability store when one is wired.
func (a *Agent) buildChildRegistry() *childRegistry {
	reg := newChildRegistry()
	for _, site := range a.defSites() {
		node := subagentNodeName(site.def)
		if reg.lookup(node) != nil {
			continue // identical content already registered under this node
		}
		def := site.def
		child, err := a.newSubagent(runws.With(context.Background(), site.ws), &def)
		if err != nil {
			if a.obs != nil {
				a.obs.Log("warn", "subagent_registry_skip", map[string]any{
					"def":       def.Name,
					"node":      node,
					"workspace": site.ws,
					"error":     err.Error(),
				})
			}
			continue
		}
		reg.set(node, child)
	}
	return reg
}

// rebuildGraph compiles the agent graph: the fixed model →(route1)→ hooks →
// approval →(route2)→ tools topology plus one subgraph node per registered
// subagent def (design §3.1/§3.2). With zero defs the compiled graph is
// identical to the pre-registry topology (no subagent nodes, no edges to
// them). Called from New — which propagates a compile failure — and from
// the debounced scheduler on def-set changes; the compiled result and the
// registry are swapped under a.mu, so in-flight runs keep the instance
// they captured at their entry point (see currentGraph).
func (a *Agent) rebuildGraph() error {
	a.rebuildBuild.Lock()
	defer a.rebuildBuild.Unlock()
	a.mu.Lock()
	hasTools := len(a.toolList) > 0
	limit := a.recursionLimit
	a.mu.Unlock()

	reg := a.buildChildRegistry()

	g := lggraph.NewStateGraph()
	g.AddReducer("messages", messagesChannel)
	g.AddReducer(decisionsChannelName, decisionsChannel)
	g.AddNode(nodeModel, a.observedNode("agent", nodeModel, a.modelNode()))
	if hasTools {
		// Topology (design §3.1): model →(route1)→ hooks → approval →(route2)
		// → tools → model. Gates/hooks live in the hooks superstep (never
		// interrupts, so it never re-executes); every user ask is ONE
		// interrupt in the approval superstep; the tools node executes
		// purely from the persisted decisions channel.
		g.AddNode(nodeHooks, a.observedNode("agent", nodeHooks, a.hooksNode()))
		g.AddNode(nodeApproval, a.observedNode("agent", nodeApproval, a.approvalNode()))
		g.AddNode(nodeTools, a.observedNode("agent", nodeTools, a.toolsNode()))
		g.AddEdge(nodeHooks, nodeApproval)
		g.AddConditionalEdges(nodeApproval, a.routeTools)
		g.AddEdge(nodeTools, nodeModel)
	}
	// Content-addressed subagent children (design §3.2): one subgraph node
	// per distinct def content. Task 7 fans foreground Task calls out to
	// these via types.Send from route2; until then the nodes sit
	// unreachable, changing nothing about execution.
	for _, node := range reg.nodes() {
		g.AddSubgraph(node, reg.lookup(node).Graph())
	}
	g.AddConditionalEdges(nodeModel, a.route)
	g.SetEntryPoint(nodeModel)

	cg, err := g.Compile(
		lggraph.WithRecursionLimit(limit),
		lggraph.WithCheckpointer(a.saver),
	)
	if err != nil {
		return fmt.Errorf("agent: compile graph: %w", err)
	}
	a.mu.Lock()
	a.children = reg
	a.graph = cg
	a.mu.Unlock()
	return nil
}

// scheduleGraphRebuild arms (or resets) the def-set change debounce. Safe
// from any goroutine, including wsCache.Set callbacks: it never blocks on
// a.mu, only on the tiny rebuildMu.
func (a *Agent) scheduleGraphRebuild() {
	a.rebuildMu.Lock()
	defer a.rebuildMu.Unlock()
	if a.rebuildTimer != nil {
		a.rebuildTimer.Reset(graphRebuildDebounce)
		return
	}
	a.rebuildTimer = time.AfterFunc(graphRebuildDebounce, func() {
		a.rebuildMu.Lock()
		a.rebuildTimer = nil
		a.rebuildMu.Unlock()
		a.rebuildNow()
	})
}

// flushGraphRebuild cancels any pending debounce and rebuilds
// synchronously: the resume path needs deterministic registry state before
// deciding a subagent node is gone, and tests need it immediately.
func (a *Agent) flushGraphRebuild() {
	a.rebuildMu.Lock()
	t := a.rebuildTimer
	a.rebuildTimer = nil
	a.rebuildMu.Unlock()
	if t != nil {
		t.Stop()
	}
	a.rebuildNow()
}

// rebuildNow runs one rebuild; failures are logged here (New reports its
// own rebuild error directly).
func (a *Agent) rebuildNow() {
	if err := a.rebuildGraph(); err != nil {
		if a.obs != nil {
			a.obs.Log("error", "graph_rebuild_failed", map[string]any{"error": err.Error()})
		}
	}
}

// childReg snapshots the current registry pointer (a.mu guards the swap in
// rebuildGraph; the registry's own lock guards its contents).
func (a *Agent) childReg() *childRegistry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.children
}

// checkResumableSubagents rejects resuming a thread whose paused
// checkpoint planned work in a subagent node that no longer exists — the
// definition was removed or edited under the pause (design §3.2). Falling
// through is never an option: the executor would re-plan the task onto a
// missing node (a generic "unknown node" error) or, worse, re-execute
// differently after a future re-registration, so the def and node are
// named explicitly and the resume aborts. Threads whose pause references
// no subagent node (every main-graph pause) pass through untouched.
func (a *Agent) checkResumableSubagents(ctx context.Context, threadID string) error {
	if a.saver == nil {
		return nil
	}
	tup, err := a.saver.GetTuple(ctx, lgcheckpoint.Config{ThreadID: threadID})
	if err != nil || tup == nil {
		return nil // no checkpoint: the graph surfaces its own resume error
	}
	for _, pt := range tup.Checkpoint.Next {
		if !strings.HasPrefix(pt.Node, subagentNodePrefix) {
			continue
		}
		if a.childReg().lookup(pt.Node) != nil {
			continue
		}
		// The def set may have changed with the debounce still pending —
		// or this process never resolved the defs at all. Rebuild once,
		// synchronously, before declaring the node gone.
		a.flushGraphRebuild()
		if a.childReg().lookup(pt.Node) != nil {
			continue
		}
		return fmt.Errorf("agent: cannot resume thread %q: its paused plan references subagent node %q (definition %q), which is no longer registered — the definition was removed or changed while the run was paused; start a new run", threadID, pt.Node, subagentDefLabel(pt.Node))
	}
	return nil
}
