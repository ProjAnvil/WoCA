package graph

import (
	"context"
	"errors"
)

// InterruptError pauses a run to request a value from the caller (human or
// another process). It carries the value the node wishes to surface and, once
// the runtime has caught it, a checkpoint from which the run can be resumed.
type InterruptError struct {
	// Value is the payload exposed to the caller (e.g. a permission request).
	Value any
	// Key is the checkpointer key for this interrupt, set when a checkpointer
	// was configured (empty otherwise).
	Key string
	// checkpoint is populated by the run loop when it catches the interrupt.
	checkpoint *checkpoint
}

func (e *InterruptError) Error() string { return "graph interrupted" }

// checkpoint records where a run paused so it can be resumed.
type checkpoint struct {
	// node is the node that interrupted; resume re-enters this node.
	node string
	// values is the graph state before the node executed.
	values State
}

// IsInterrupt reports whether err is an interrupt.
func IsInterrupt(err error) bool {
	var ie *InterruptError
	return errors.As(err, &ie)
}

// AsInterrupt unwraps err into *InterruptError when it is an interrupt.
func AsInterrupt(err error) (*InterruptError, bool) {
	var ie *InterruptError
	if errors.As(err, &ie) {
		return ie, true
	}
	return nil, false
}

// Interrupt pauses the graph and requests a value. On the first pass it
// returns a non-nil error that the node must propagate:
//
//	answer, err := graph.Interrupt(ctx, "continue?")
//	if err != nil {
//		return nil, err
//	}
//	// resumed: answer holds the value supplied on resume.
//
// When the run is resumed, the interrupted node is re-executed from its
// checkpoint and Interrupt returns the resume value with a nil error.
func Interrupt(ctx context.Context, value any) (any, error) {
	if rs := resumeFromContext(ctx); rs != nil && len(rs.values) > 0 {
		v := rs.values[0]
		rs.values = rs.values[1:]
		return v, nil
	}
	return nil, &InterruptError{Value: value}
}

type resumeCtxKey struct{}

type resumeContext struct {
	values []any
}

func withResumeValue(ctx context.Context, value any) context.Context {
	return context.WithValue(ctx, resumeCtxKey{}, &resumeContext{values: []any{value}})
}

func resumeFromContext(ctx context.Context) *resumeContext {
	rs, _ := ctx.Value(resumeCtxKey{}).(*resumeContext)
	return rs
}
