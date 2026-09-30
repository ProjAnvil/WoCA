// Processing plugins (P1 extension surface): in-process tool guards and
// model hooks injected at assembly time (agent.Config). They share the
// event vocabulary with the settings-file hooks but run in-process — the
// embedding host's extension point (sdk re-exports the types). Ordering:
// guards and hooks run BEFORE the settings-file hooks of the same event
// (in-process code is host-authoritative), in registration order, and any
// deny wins.
package agent

import (
	"context"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
)

// ToolAction is a tool guard's verdict on one tool call.
type ToolAction int

const (
	ToolAllow ToolAction = iota // proceed to the normal permission gate
	ToolDeny                    // skip execution; Reason is fed to the model
	ToolAsk                     // escalate to the permission prompt (interrupt)
)

// ToolDecision is a guard's BeforeTool verdict. The zero value allows.
type ToolDecision struct {
	Action ToolAction
	// Reason is the deny feedback: it becomes the tool result the model
	// sees ("Error: <reason>").
	Reason string
}

// AllowTool proceeds through the normal permission gate.
func AllowTool() ToolDecision { return ToolDecision{Action: ToolAllow} }

// DenyTool skips the tool call and feeds reason to the model.
func DenyTool(reason string) ToolDecision { return ToolDecision{Action: ToolDeny, Reason: reason} }

// AskTool escalates the call to the permission prompt.
func AskTool() ToolDecision { return ToolDecision{Action: ToolAsk} }

// ToolCallInfo describes one tool invocation.
type ToolCallInfo struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolResultInfo describes a completed tool invocation.
type ToolResultInfo struct {
	ID      string
	Name    string
	Args    map[string]any
	Content string
}

// ToolPostDecision is a guard's AfterTool outcome. The zero value is a no-op.
type ToolPostDecision struct {
	// Feedback is appended to the tool result the model sees.
	Feedback string
}

// ToolFeedback appends s to the tool result.
func ToolFeedback(s string) ToolPostDecision { return ToolPostDecision{Feedback: s} }

// ToolGuard gates tool invocations. BeforeTool runs after the permission
// gate and before execution (speculative execution is disabled while guards
// are installed, so nothing starts behind their back); Deny skips the call,
// Ask escalates to the permission interrupt. AfterTool runs after
// execution and may append feedback to the result.
type ToolGuard interface {
	BeforeTool(ctx context.Context, call ToolCallInfo) ToolDecision
	AfterTool(ctx context.Context, call ToolCallInfo, result ToolResultInfo) ToolPostDecision
}

// ModelTurn describes the model request a ModelHook sees.
type ModelTurn struct {
	// Model is the active model id.
	Model string
	// Prompt is the last user prompt of the request (informational).
	Prompt string
}

// ModelReply is one assistant reply: the text AfterModel may replace, plus
// the model's tool calls (informational).
type ModelReply struct {
	Text      string
	ToolCalls []ToolCallInfo
}

// BeforeModelResult is a ModelHook's BeforeModel outcome. The zero value
// proceeds unchanged.
type BeforeModelResult struct {
	// Reply, when set, skips the model call: its Text becomes the
	// assistant message for this round (the first hook to set it wins).
	Reply *ModelReply
	// PrependContext is injected ahead of the request transcript for this
	// call only (the RAG/memory attachment point); it never enters the
	// persisted transcript. Multiple hooks' contexts concatenate in order.
	PrependContext string
}

// ModelHook observes and transforms model rounds. BeforeModel runs before
// each model request (return a Reply to short-circuit, PrependContext to
// attach request-scoped context). AfterModel runs after the reply
// completes — while any model hook is installed the model node buffers its
// text (no live token streaming) so replacement is consistent: nil keeps
// the reply, a non-nil reply replaces the TEXT (the model's tool calls
// survive; gate them with a ToolGuard); later hooks replace earlier
// replacements.
type ModelHook interface {
	BeforeModel(ctx context.Context, turn *ModelTurn) BeforeModelResult
	AfterModel(ctx context.Context, turn *ModelTurn, reply *ModelReply) *ModelReply
}

// guardsBeforeTool runs every guard's BeforeTool for one call. Deny wins
// over ask; the first non-allow verdict is returned.
func (a *Agent) guardsBeforeTool(ctx context.Context, call messages.ToolCall) ToolDecision {
	info := ToolCallInfo{ID: call.ID, Name: call.Name, Args: call.Args}
	var asked bool
	for _, g := range a.toolGuards {
		d := g.BeforeTool(ctx, info)
		switch d.Action {
		case ToolDeny:
			return d
		case ToolAsk:
			asked = true
		}
	}
	if asked {
		return AskTool()
	}
	return AllowTool()
}

// guardsAfterTool runs every guard's AfterTool, concatenating feedback.
func (a *Agent) guardsAfterTool(ctx context.Context, call messages.ToolCall, content string) string {
	info := ToolCallInfo{ID: call.ID, Name: call.Name, Args: call.Args}
	var fb []string
	for _, g := range a.toolGuards {
		if d := g.AfterTool(ctx, info, ToolResultInfo{ID: call.ID, Name: call.Name, Args: call.Args, Content: content}); d.Feedback != "" {
			fb = append(fb, strings.TrimSpace(d.Feedback))
		}
	}
	return strings.Join(fb, "\n")
}

// withInjectedContext copies msgs with a context block inserted right after
// the leading system messages — request-scoped only, never persisted.
func withInjectedContext(msgs []messages.Message, text string) []messages.Message {
	out := make([]messages.Message, 0, len(msgs)+1)
	i := 0
	for i < len(msgs) && msgs[i].Role == messages.RoleSystem {
		out = append(out, msgs[i])
		i++
	}
	out = append(out, messages.Human("<context>\n"+strings.TrimSpace(text)+"\n</context>"))
	out = append(out, msgs[i:]...)
	return out
}

// lastHumanPrompt returns the last human message's text ("" when none).
func lastHumanPrompt(msgs []messages.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleHuman {
			return msgs[i].Content
		}
	}
	return ""
}
