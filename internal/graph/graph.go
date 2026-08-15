package graph

import (
	"context"
	"fmt"
)

// START and END are the reserved sentinel nodes of a graph. START is the
// implicit source of the entry edge; END is the terminal node.
const (
	START = "__start__"
	END   = "__end__"
)

// NodeFunc is a graph node: it reads the current state and returns either a
// partial State update (merged via reducers), nil (no update), or *Command.
type NodeFunc func(ctx context.Context, state State) (any, error)

// StreamingNodeFunc is a node that can emit incremental deltas (e.g. model
// token deltas) as it runs. emit may be nil (non-streaming Invoke), in which
// case the node should run without emitting.
type StreamingNodeFunc func(ctx context.Context, state State, emit func(delta any)) (any, error)

// RouterFunc routes from a node to the next node (or END) based on state.
type RouterFunc func(ctx context.Context, state State) (string, error)

// Command lets a node return a state update together with a routing override.
// When Goto is non-empty it short-circuits normal edge routing.
type Command struct {
	Update State
	Goto   string
}

// StateGraph is a mutable builder for a graph. It is compiled once and then
// executed many times.
type StateGraph struct {
	nodes       map[string]NodeFunc
	streamNodes map[string]StreamingNodeFunc
	edges       map[string]string
	routers     map[string]RouterFunc
	reducers    map[string]Reducer
	entry       string
}

// NewStateGraph creates an empty graph builder.
func NewStateGraph() *StateGraph {
	return &StateGraph{
		nodes:       map[string]NodeFunc{},
		streamNodes: map[string]StreamingNodeFunc{},
		edges:       map[string]string{},
		routers:     map[string]RouterFunc{},
		reducers:    map[string]Reducer{},
	}
}

// AddNode registers a node function under name.
func (g *StateGraph) AddNode(name string, fn NodeFunc) *StateGraph {
	if name == "" {
		panic("graph: node name is required")
	}
	if fn == nil {
		panic("graph: node function is required")
	}
	g.nodes[name] = fn
	return g
}

// AddStreamingNode registers a node that can emit incremental deltas during
// streaming execution.
func (g *StateGraph) AddStreamingNode(name string, fn StreamingNodeFunc) *StateGraph {
	if name == "" {
		panic("graph: node name is required")
	}
	if fn == nil {
		panic("graph: streaming node function is required")
	}
	g.streamNodes[name] = fn
	return g
}

// AddEdge adds an unconditional edge from -> to.
func (g *StateGraph) AddEdge(from, to string) *StateGraph {
	g.edges[from] = to
	return g
}

// AddConditionalEdges adds a conditional edge from -> fn. The router decides
// the next node dynamically.
func (g *StateGraph) AddConditionalEdges(from string, fn RouterFunc) *StateGraph {
	if fn == nil {
		panic("graph: router function is required")
	}
	g.routers[from] = fn
	return g
}

// AddReducer declares a reducer for a state channel. A nil reducer is treated
// as Replace (the default for any undeclared channel).
func (g *StateGraph) AddReducer(key string, r Reducer) *StateGraph {
	if r == nil {
		r = Replace
	}
	g.reducers[key] = r
	return g
}

// SetEntryPoint marks the node the graph starts from.
func (g *StateGraph) SetEntryPoint(name string) *StateGraph {
	g.entry = name
	return g
}

// Compile validates the graph and returns an executable CompiledGraph.
func (g *StateGraph) Compile(opts ...CompileOption) (*CompiledGraph, error) {
	cfg := compileConfig{recursionLimit: defaultRecursionLimit}
	for _, opt := range opts {
		opt(&cfg)
	}
	if g.entry == "" {
		return nil, fmt.Errorf("graph: entry point is required")
	}
	if !g.hasNode(g.entry) {
		return nil, fmt.Errorf("graph: entry node %q is not defined", g.entry)
	}
	if err := validateEdges(g); err != nil {
		return nil, err
	}
	return &CompiledGraph{
		nodes:          g.nodes,
		streamNodes:    g.streamNodes,
		edges:          g.edges,
		routers:        g.routers,
		reducers:       g.reducers,
		entry:          g.entry,
		recursionLimit: cfg.recursionLimit,
		checkpointer:   cfg.checkpointer,
	}, nil
}

func (g *StateGraph) hasNode(name string) bool {
	if _, ok := g.nodes[name]; ok {
		return true
	}
	_, ok := g.streamNodes[name]
	return ok
}

func validateEdges(g *StateGraph) error {
	known := func(name string) bool {
		if name == START || name == END {
			return true
		}
		return g.hasNode(name)
	}
	for from, to := range g.edges {
		if from != START && !known(from) {
			return fmt.Errorf("graph: edge source %q is not a known node", from)
		}
		if !known(to) {
			return fmt.Errorf("graph: edge target %q is not a known node", to)
		}
	}
	for from := range g.routers {
		if from != START && !known(from) {
			return fmt.Errorf("graph: conditional edge source %q is not a known node", from)
		}
	}
	return nil
}
