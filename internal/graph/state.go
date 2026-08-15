// Package graph implements a langgraph-style StateGraph runtime in Go.
//
// It provides the core abstractions of LangGraph: a graph of named nodes
// connected by (optionally conditional) edges, a shared state made of named
// channels, per-channel reducers for merging node updates, a compile step,
// streaming execution, recursion limits, and human-in-the-loop interrupt /
// resume via checkpoints.
//
// The design intentionally mirrors the public surface of LangGraph's
// StateGraph API (AddNode, AddEdge, AddConditionalEdges, AddReducer,
// SetEntryPoint, Compile, Invoke, Stream, Interrupt) so that an agent can be
// expressed as an explicit graph instead of a bespoke loop.
package graph

import (
	"reflect"
)

// State is a single graph-state snapshot: a set of named channels, each
// holding an arbitrary Go value.
type State map[string]any

// Reducer combines the existing value of a channel with an incoming update
// produced by a node, returning the new combined value.
type Reducer func(existing, update any) any

// Replace implements last-value-wins semantics: the update overwrites the
// previous value. This is LangGraph's default reducer for an undeclared
// channel.
func Replace(_, update any) any {
	return update
}

// Append concatenates slice updates. Both existing and update must be slices
// of the same element type (when existing is nil the update is returned
// as-is). Non-slice values fall back to replace semantics.
func Append(existing, update any) any {
	if existing == nil {
		return update
	}
	ev := reflect.ValueOf(existing)
	uv := reflect.ValueOf(update)
	if ev.Kind() != reflect.Slice || uv.Kind() != reflect.Slice {
		return update
	}
	out := reflect.MakeSlice(ev.Type(), 0, ev.Len()+uv.Len())
	out = reflect.AppendSlice(out, ev)
	out = reflect.AppendSlice(out, uv)
	return out.Interface()
}

// cloneState returns a shallow copy of a state map. Channel values are shared
// (reducers are expected to produce new values rather than mutate in place).
func cloneState(state State) State {
	if state == nil {
		return State{}
	}
	out := make(State, len(state))
	for k, v := range state {
		out[k] = v
	}
	return out
}
