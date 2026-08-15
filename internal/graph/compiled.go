package graph

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const defaultRecursionLimit = 25

// compileConfig holds options applied at Compile time.
type compileConfig struct {
	recursionLimit int
	checkpointer   Checkpointer
	obsName        string
	observer       Observer
}

// CompileOption configures compilation.
type CompileOption func(*compileConfig)

// WithRecursionLimit caps the number of node dispatches per run.
func WithRecursionLimit(limit int) CompileOption {
	return func(c *compileConfig) { c.recursionLimit = limit }
}

// WithObserver installs a per-dispatch observer under a graph name (used by
// the harness observability log).
func WithObserver(name string, o Observer) CompileOption {
	return func(c *compileConfig) { c.obsName, c.observer = name, o }
}

// WithCheckpointer installs a checkpointer for interrupt/resume persistence.
func WithCheckpointer(cp Checkpointer) CompileOption {
	return func(c *compileConfig) { c.checkpointer = cp }
}

// Checkpointer persists and restores checkpoints so a paused run can be
// resumed. The in-memory implementation is sufficient for a single process;
// swap in a durable backend to survive restarts.
type Checkpointer interface {
	Put(key string, cp checkpoint) error
	Get(key string) (checkpoint, bool)
}

// MemoryCheckpointer is an in-process checkpointer.
type MemoryCheckpointer struct {
	checkpoints map[string]checkpoint
}

// NewMemoryCheckpointer creates an empty in-memory checkpointer.
func NewMemoryCheckpointer() *MemoryCheckpointer {
	return &MemoryCheckpointer{checkpoints: map[string]checkpoint{}}
}

// Put stores a checkpoint under key.
func (m *MemoryCheckpointer) Put(key string, cp checkpoint) error {
	m.checkpoints[key] = cp
	return nil
}

// Get retrieves a checkpoint under key.
func (m *MemoryCheckpointer) Get(key string) (checkpoint, bool) {
	cp, ok := m.checkpoints[key]
	return cp, ok
}

// CompiledGraph is an executable graph built by StateGraph.Compile.
type CompiledGraph struct {
	nodes       map[string]NodeFunc
	streamNodes map[string]StreamingNodeFunc
	edges       map[string]string
	routers     map[string]RouterFunc
	reducers    map[string]Reducer
	entry       string

	recursionLimit int
	checkpointer   Checkpointer
	obsName        string
	observer       Observer
}

// Step is one unit of streamed progress. Node identifies the node that just
// ran; Update is the delta it produced; Values is the full state after the
// update; Delta is an incremental delta emitted by a streaming node (e.g. a
// text token). A terminal step has Done set (and empty Node) or a non-nil Err.
type Step struct {
	Node   string
	Update State
	Values State
	Delta  any
	Done   bool
	Err    error
}

// Stream is a pull-based stream of graph progress events.
type Stream[T any] interface {
	Next(ctx context.Context) (T, bool, error)
	Close() error
}

// runOpts carries optional resume parameters for a run.
type runOpts struct {
	startNode   string
	resumeNode  string
	resumeValue any
}

// Invoke runs the graph to completion and returns the final state.
func (g *CompiledGraph) Invoke(ctx context.Context, input State) (State, error) {
	return g.runInternal(ctx, input, runOpts{}, nil)
}

// Resume continues a paused run from an interrupt, feeding resumeValue back
// into the interrupted node.
func (g *CompiledGraph) Resume(ctx context.Context, ie *InterruptError, resumeValue any) (State, error) {
	if ie == nil || ie.checkpoint == nil {
		return nil, fmt.Errorf("graph: resume requires an interrupt error with a checkpoint")
	}
	cp := *ie.checkpoint
	return g.runInternal(ctx, cp.values, runOpts{
		startNode:   cp.node,
		resumeNode:  cp.node,
		resumeValue: resumeValue,
	}, nil)
}

// ResumeCheckpoint resumes a run from a persisted checkpoint key.
func (g *CompiledGraph) ResumeCheckpoint(ctx context.Context, key string, resumeValue any) (State, error) {
	if g.checkpointer == nil {
		return nil, fmt.Errorf("graph: resume checkpoint requires a checkpointer")
	}
	cp, ok := g.checkpointer.Get(key)
	if !ok {
		return nil, fmt.Errorf("graph: checkpoint %q not found", key)
	}
	return g.runInternal(ctx, cp.values, runOpts{
		startNode:   cp.node,
		resumeNode:  cp.node,
		resumeValue: resumeValue,
	}, nil)
}

// Stream runs the graph and streams one Step per node dispatch, ending with a
// terminal step carrying the final state (Done) or the error.
func (g *CompiledGraph) Stream(ctx context.Context, input State) (Stream[Step], error) {
	return g.streamInternal(ctx, input, runOpts{})
}

// StreamResume continues a paused run from an interrupt as a stream.
func (g *CompiledGraph) StreamResume(ctx context.Context, ie *InterruptError, resumeValue any) (Stream[Step], error) {
	if ie == nil || ie.checkpoint == nil {
		return nil, fmt.Errorf("graph: resume requires an interrupt error with a checkpoint")
	}
	cp := *ie.checkpoint
	return g.streamInternal(ctx, cp.values, runOpts{
		startNode:   cp.node,
		resumeNode:  cp.node,
		resumeValue: resumeValue,
	})
}

// runInternal is the shared execution loop. emit is optional (nil for
// non-streaming Invoke). Errors are returned to the caller and never emitted
// through emit; streamInternal emits the single terminal error step.
func (g *CompiledGraph) runInternal(ctx context.Context, input State, opts runOpts, emit func(Step)) (State, error) {
	state := cloneState(input)
	node := g.entry
	if opts.startNode != "" {
		node = opts.startNode
	}

	steps := 0
	for {
		if node == END || node == "" {
			return state, nil
		}
		steps++
		if g.recursionLimit > 0 && steps > g.recursionLimit {
			return nil, fmt.Errorf("graph: recursion limit %d exceeded at node %q", g.recursionLimit, node)
		}

		runCtx := ctx
		if opts.resumeNode != "" && opts.resumeNode == node {
			runCtx = withResumeValue(ctx, opts.resumeValue)
			opts.resumeNode = ""
		}

		if g.observer != nil {
			g.observer.ObserveNode(NodeEvent{Graph: g.obsName, Node: node, Phase: PhaseStart, Values: state})
		}
		start := time.Now()
		out, err := g.dispatch(runCtx, node, state, emit)
		if g.observer != nil {
			phase, errText := PhaseEnd, ""
			if err != nil {
				errText = err.Error()
				if _, isInterrupt := AsInterrupt(err); isInterrupt {
					phase = PhaseInterrupt
				} else {
					phase = PhaseError
				}
			}
			g.observer.ObserveNode(NodeEvent{
				Graph:      g.obsName,
				Node:       node,
				Phase:      phase,
				DurationMS: float64(time.Since(start).Nanoseconds()) / 1e6,
				Err:        errText,
				Update:     out,
			})
		}
		if err != nil {
			var ie *InterruptError
			if errors.As(err, &ie) {
				ckpt := &checkpoint{node: node, values: cloneState(state)}
				ie.checkpoint = ckpt
				if g.checkpointer != nil {
					key := newCheckpointKey()
					if putErr := g.checkpointer.Put(key, *ckpt); putErr == nil {
						ie.Key = key
					}
					// A failed Put leaves Key empty; the in-memory Resume path
					// remains available, but cross-process resume is not.
				}
				return state, ie
			}
			return nil, err
		}

		var update State
		gotoOverride := ""
		switch v := out.(type) {
		case nil:
			// no update
		case State:
			update = v
			state = g.merge(state, v)
		case Command:
			update = v.Update
			state = g.merge(state, v.Update)
			gotoOverride = v.Goto
		case *Command:
			update = v.Update
			state = g.merge(state, v.Update)
			gotoOverride = v.Goto
		default:
			return nil, fmt.Errorf("graph: node %q returned unsupported value %T", node, out)
		}

		if emit != nil {
			emit(Step{Node: node, Update: update, Values: state})
		}

		if gotoOverride != "" {
			node = gotoOverride
			continue
		}
		next, err := g.route(ctx, node, state)
		if err != nil {
			return nil, err
		}
		node = next
	}
}

// dispatch runs a node, routing to a streaming node when available and
// forwarding its deltas through emit.
func (g *CompiledGraph) dispatch(ctx context.Context, node string, state State, emit func(Step)) (any, error) {
	if sfn, ok := g.streamNodes[node]; ok {
		return sfn(ctx, state, func(delta any) {
			if emit != nil {
				emit(Step{Node: node, Delta: delta, Values: state})
			}
		})
	}
	fn, ok := g.nodes[node]
	if !ok {
		return nil, fmt.Errorf("graph: node %q is not defined", node)
	}
	return fn(ctx, state)
}

// merge applies a partial update to state using per-channel reducers.
func (g *CompiledGraph) merge(state State, update State) State {
	out := cloneState(state)
	for k, v := range update {
		if existing, ok := out[k]; ok {
			if r, ok := g.reducers[k]; ok {
				out[k] = r(existing, v)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// route resolves the next node after node has executed.
func (g *CompiledGraph) route(ctx context.Context, node string, state State) (string, error) {
	if r, ok := g.routers[node]; ok {
		return r(ctx, state)
	}
	if to, ok := g.edges[node]; ok {
		return to, nil
	}
	return END, nil
}

// streamInternal wraps runInternal in a pull-based Stream.
func (g *CompiledGraph) streamInternal(ctx context.Context, input State, opts runOpts) (Stream[Step], error) {
	runCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Step, 16)
	s := &stepStream{ch: ch, cancel: cancel}

	go func() {
		defer close(ch)
		defer cancel()
		final, err := g.runInternal(runCtx, input, opts, func(step Step) {
			select {
			case ch <- step:
			case <-runCtx.Done():
			}
		})
		if err != nil {
			select {
			case ch <- Step{Values: final, Err: err}:
			case <-runCtx.Done():
			}
			return
		}
		select {
		case ch <- Step{Values: final, Done: true}:
		case <-runCtx.Done():
		}
	}()
	return s, nil
}

type stepStream struct {
	ch     chan Step
	cancel context.CancelFunc
}

func (s *stepStream) Next(ctx context.Context) (Step, bool, error) {
	select {
	case step, ok := <-s.ch:
		if !ok {
			return Step{}, false, nil
		}
		return step, true, nil
	case <-ctx.Done():
		return Step{}, false, ctx.Err()
	}
}

func (s *stepStream) Close() error {
	s.cancel()
	return nil
}
