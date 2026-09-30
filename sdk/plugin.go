package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Plugin is the root interface for woca processing plugins. Plugins are
// injected at construction time (WithPlugins) — one assembly, reused for
// the whole session — and must be safe for concurrent use.
//
// Power today is observe-only: an EventPlugin sees the run lifecycle as it
// streams. Gating and transformation hooks (tool guards, model hooks) are
// planned follow-ups; a plugin implementing only Plugin is accepted and
// ignored until then.
type Plugin interface {
	Name() string
}

// RunEventKind classifies run-lifecycle observations.
type RunEventKind string

const (
	RunEventStart   RunEventKind = "run_start" // before the session hooks run; Prompt is set
	RunEventMessage RunEventKind = "message"   // one transcript message: assistant text/tool calls, or a tool result
	RunEventEnd     RunEventKind = "run_end"   // terminal envelope; Result is set
)

// RunEvent is one run-lifecycle observation delivered to EventPlugins.
// Exactly one of Prompt / Message / Result is set per Kind; Message and
// Result are the same types the Events channel streams, so one parser
// serves both surfaces.
type RunEvent struct {
	Kind      RunEventKind
	SessionID string
	RunID     string
	Prompt    string   // RunEventStart only: the prompt as the host passed it
	Message   *Message // RunEventMessage only
	Result    *Result  // RunEventEnd only
	At        time.Time
}

// EventPlugin observes the run lifecycle. Observe is called sequentially in
// stream order from the run goroutine — keep it fast or dispatch
// internally; a slow plugin delays the run.
type EventPlugin interface {
	Plugin
	Observe(ctx context.Context, ev RunEvent)
}

// dispatchRunEvent delivers ev to every observer, in plugin order.
func dispatchRunEvent(ctx context.Context, observers []EventPlugin, ev RunEvent) {
	for _, o := range observers {
		o.Observe(ctx, ev)
	}
}

// eventObservers extracts the EventPlugin views of the configured plugins.
func eventObservers(ps []Plugin) []EventPlugin {
	var out []EventPlugin
	for _, p := range ps {
		if ep, ok := p.(EventPlugin); ok {
			out = append(out, ep)
		}
	}
	return out
}

// newID returns a random 16-byte hex identity for run/session correlation.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; degrade to a
		// padded nanosecond id rather than panicking in a reporting path.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
