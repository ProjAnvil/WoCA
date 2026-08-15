package graph

// Observer receives one event per node dispatch of a compiled graph — the
// observability hook for harnesses that trace graph and state transitions
// (woca's obslog records them as session-trace events). A nil observer (the
// default) costs nothing.
type Observer interface {
	ObserveNode(ev NodeEvent)
}

// Node phases.
const (
	PhaseStart     = "start"
	PhaseEnd       = "end"
	PhaseInterrupt = "interrupt"
	PhaseError     = "error"
)

// NodeEvent describes one node dispatch. Update carries the node's raw
// return value (nil, State, *Command, or Command); Values is the state the
// node saw (start events only).
type NodeEvent struct {
	Graph      string  // graph name given to WithObserver
	Node       string  // node name
	Phase      string  // start | end | interrupt | error
	DurationMS float64 // dispatch duration (end/interrupt/error events)
	Err        string  // error text (interrupt/error events)
	Update     any     // the node's return value (end events)
	Values     State   // state entering the node (start events)
}
