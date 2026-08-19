// Package eventbus defines the platform event bus interface used for
// fan-out of typed, sequenced events (e.g. SSE streams with Last-Event-ID
// resume). Business code programs against Bus; the local implementation
// preserves current single-node behavior and future remote implementations
// (NATS/gRPC) live in their own subpackages. This package depends on the
// standard library only.
package eventbus

import (
	"context"
	"errors"
)

// ErrClosed is returned by Publish and Subscribe after the bus is closed.
var ErrClosed = errors.New("eventbus: closed")

// Event is a single typed occurrence on a topic. Seq is assigned by the bus
// per topic, starting at 1 and increasing monotonically; it is the value a
// client feeds back as sinceSeq (the SSE Last-Event-ID) to resume a stream.
type Event struct {
	Type string
	Data any
	Seq  int64
}

// Bus publishes and fans out events on named topics. Implementations must be
// safe for concurrent use.
type Bus interface {
	// Publish appends ev to the topic's history and fans it out to live
	// subscribers. The bus assigns ev.Seq (any caller-supplied value is
	// ignored) and returns it. Publishing never blocks on slow subscribers:
	// events that do not fit a subscriber's buffer are dropped, and the
	// client recovers the gap by resubscribing with sinceSeq.
	// Implementations may bound per-topic history retention, dropping the
	// oldest events once the bound is exceeded.
	Publish(ctx context.Context, topic string, ev Event) (int64, error)
	// Subscribe replays the topic's retained history with seq > sinceSeq in
	// order, then delivers live events. sinceSeq = 0 replays everything;
	// sinceSeq < 0 subscribes live-only (no history replay), resolved
	// atomically against the topic head so no event is missed.
	Subscribe(ctx context.Context, topic string, sinceSeq int64) (*Subscription, error)
	// Close shuts the bus down: all active subscription channels are closed
	// and later Publish/Subscribe calls return ErrClosed.
	Close() error
}
