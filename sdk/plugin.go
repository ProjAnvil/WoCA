package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/projanvil/langchain-golang/core/language"

	"github.com/projanvil/woca/internal/agent"
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

// Gating and transformation capabilities, re-exported from the harness as
// aliases: one plugin may implement EventPlugin, ToolGuard, and ModelHook
// in any combination. ToolGuards run after the permission gate and before
// execution (any guard installed disables speculative tool execution);
// ModelHooks transform model rounds (installing any one buffers the
// model-node stream so reply replacement stays consistent).
type (
	ToolGuard         = agent.ToolGuard
	ToolAction        = agent.ToolAction
	ToolDecision      = agent.ToolDecision
	ToolCallInfo      = agent.ToolCallInfo
	ToolResultInfo    = agent.ToolResultInfo
	ToolPostDecision  = agent.ToolPostDecision
	ModelHook         = agent.ModelHook
	ModelTurn         = agent.ModelTurn
	ModelReply        = agent.ModelReply
	BeforeModelResult = agent.BeforeModelResult
)

const (
	ToolAllow = agent.ToolAllow
	ToolDeny  = agent.ToolDeny
	ToolAsk   = agent.ToolAsk
)

var (
	AllowTool    = agent.AllowTool
	DenyTool     = agent.DenyTool
	AskTool      = agent.AskTool
	ToolFeedback = agent.ToolFeedback
)

// ChatModel is the model interface wrappers receive and return (an alias
// of the harness's language binding — see ModelWrapper).
type ChatModel = language.ChatModel

// ModelWrapper wraps the chat model at assembly time and on every tool
// rebind — the call-level capability: caching, retries, fan-out, alternate
// backends. Wrappers sit inside the chat-span decorator (a cache hit still
// records its span; put UsageMetadata on synthesized replies). The first
// registered wrapper is the outermost.
type ModelWrapper = agent.ModelWrapper

// toolGuardsOf extracts the ToolGuard views of the configured plugins.
func toolGuardsOf(ps []Plugin) []agent.ToolGuard {
	var out []agent.ToolGuard
	for _, p := range ps {
		if g, ok := p.(agent.ToolGuard); ok {
			out = append(out, g)
		}
	}
	return out
}

// modelHooksOf extracts the ModelHook views of the configured plugins.
func modelHooksOf(ps []Plugin) []agent.ModelHook {
	var out []agent.ModelHook
	for _, p := range ps {
		if h, ok := p.(agent.ModelHook); ok {
			out = append(out, h)
		}
	}
	return out
}

// modelWrappersOf extracts the ModelWrapper views of the configured plugins.
func modelWrappersOf(ps []Plugin) []agent.ModelWrapper {
	var out []agent.ModelWrapper
	for _, p := range ps {
		if w, ok := p.(agent.ModelWrapper); ok {
			out = append(out, w)
		}
	}
	return out
}
