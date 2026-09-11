// Package sdk is the public, in-process Go SDK for the woca agent harness:
// one Query call assembles the same agent the CLI runs (model, tools, hooks,
// skills, MCP, permissions) and streams its progress as typed events.
//
// Interrupt resume-value convention: a custom OnInterrupt handler returns
// "y" or "n" for permission and plan_approval interrupts, and
// agent.AskUserAnswer (answers in question order) for ask_user interrupts.
// The SDK keeps resume values thin and does not wrap these types further.
package sdk

import (
	"encoding/json"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/cost"
)

// EventType classifies a streamed event.
type EventType string

const (
	EventAssistant EventType = "assistant" // model message (text and/or tool calls)
	EventUser      EventType = "user"      // tool result message
	EventResult    EventType = "result"    // terminal envelope, always the last event
)

// ToolCall is one model-requested tool invocation.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Message is an assistant or tool-result transcript message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// Usage mirrors agent.UsageTotals (token accounting for the whole run).
type Usage struct {
	Requests         int `json:"requests"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Result is the terminal envelope of a run.
type Result struct {
	Subtype   string   `json:"subtype"` // success | error
	Text      string   `json:"result,omitempty"`
	Usage     Usage    `json:"usage"`
	CostUSD   *float64 `json:"total_cost_usd,omitempty"`
	NumTurns  int      `json:"num_turns,omitempty"`
	IsError   bool     `json:"is_error,omitempty"`
	ErrorText string   `json:"error,omitempty"`
}

// Event is one streamed item: exactly one of Message / Result is set.
type Event struct {
	Type    EventType
	Message *Message
	Result  *Result
}

// messageFrom converts an internal transcript message to the public shape.
// Role is normalized to the stream-json wire values ("assistant" / "user").
func messageFrom(m messages.Message) *Message {
	role := string(m.Role)
	switch m.Role {
	case messages.RoleAI:
		role = "assistant"
	case messages.RoleTool:
		role = "user"
	}
	msg := &Message{Role: role, Content: m.Content, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args})
	}
	return msg
}

// resultEvent builds the terminal event from a finished run, mirroring
// sdkout.Writer.Final (usage, cost estimate, last AI text, error marking).
func resultEvent(transcript []messages.Message, u agent.UsageTotals, model string, numTurns int, runErr error) Event {
	r := &Result{Usage: Usage{
		Requests:         u.Requests,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}, NumTurns: numTurns}
	if est, known := cost.Estimate(model, cost.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
	}); known {
		r.CostUSD = &est
	}
	if runErr != nil {
		r.Subtype = "error"
		r.IsError = true
		r.ErrorText = runErr.Error()
	} else {
		r.Subtype = "success"
		r.Text = lastAIText(transcript)
	}
	return Event{Type: EventResult, Result: r}
}

// lastAIText returns the content of the last non-empty assistant message.
func lastAIText(transcript []messages.Message) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == messages.RoleAI && transcript[i].Content != "" {
			return transcript[i].Content
		}
	}
	return ""
}

// WireJSON serializes the event in the flattened stream-json shape,
// semantically compatible with sdkout's stream-json lines (same keys and
// values; key order may differ) — the format eval/woca_agent.py parses.
func (e Event) WireJSON() ([]byte, error) {
	wire := map[string]any{"type": string(e.Type)}
	if e.Message != nil {
		data, err := json.Marshal(e.Message)
		if err != nil {
			return nil, err
		}
		wire["message"] = json.RawMessage(data)
	}
	if r := e.Result; r != nil {
		wire["subtype"] = r.Subtype
		if r.Text != "" {
			wire["result"] = r.Text
		}
		usage, err := json.Marshal(r.Usage)
		if err != nil {
			return nil, err
		}
		wire["usage"] = json.RawMessage(usage)
		if r.CostUSD != nil {
			wire["total_cost_usd"] = *r.CostUSD
		}
		if r.NumTurns != 0 {
			wire["num_turns"] = r.NumTurns
		}
		if r.IsError {
			wire["is_error"] = true
		}
		if r.ErrorText != "" {
			wire["error"] = r.ErrorText
		}
	}
	return json.Marshal(wire)
}
