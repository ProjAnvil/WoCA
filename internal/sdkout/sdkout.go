// Package sdkout renders agent runs as machine-readable output for headless
// SDK consumption. "json" prints one result object after the run;
// "stream-json" prints one JSON object per line as events complete (assistant
// messages, tool results, then the final result envelope).
package sdkout

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/cost"
)

// Formats.
const (
	FormatJSON       = "json"
	FormatStreamJSON = "stream-json"
)

// Valid reports whether format is a supported output format.
func Valid(format string) bool {
	return format == "" || format == FormatJSON || format == FormatStreamJSON
}

// Writer serializes run events in the configured format.
type Writer struct {
	mu     sync.Mutex
	format string
	w      io.Writer
}

// New builds a Writer ("" behaves like json: final object only).
func New(format string, w io.Writer) (*Writer, error) {
	if !Valid(format) {
		return nil, fmt.Errorf("sdkout: unknown format %q (json | stream-json)", format)
	}
	if format == "" {
		format = FormatJSON
	}
	return &Writer{format: format, w: w}, nil
}

// event is one JSON line.
type event struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
	Subtype string          `json:"subtype,omitempty"`
	// Result fields (type=="result").
	Result    string          `json:"result,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	CostUSD   *float64        `json:"total_cost_usd,omitempty"`
	NumTurns  int             `json:"num_turns,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	ErrorText string          `json:"error,omitempty"`
}

// Step emits one stream chunk as stream events (stream-json only). Only
// update chunks carry transcript messages; custom payloads (token deltas)
// have no stream-json event.
func (w *Writer) Step(chunk lggraph.StreamChunk) {
	if w.format != FormatStreamJSON || chunk.Mode != lggraph.StreamUpdates {
		return
	}
	updates, _ := chunk.Payload.(map[string]any)
	for _, nodeUpdate := range updates {
		state, _ := nodeUpdate.(map[string]any)
		for _, m := range agent.MessagesFrom(state) {
			switch m.Role {
			case messages.RoleAI:
				if m.Content == "" && len(m.ToolCalls) == 0 {
					continue
				}
				w.write(event{Type: "assistant", Message: messageJSON(m)})
			case messages.RoleTool:
				w.write(event{Type: "user", Message: messageJSON(m)})
			}
		}
	}
}

// messageJSON serializes a message payload for an event.
func messageJSON(m messages.Message) json.RawMessage {
	payload := map[string]any{"role": string(m.Role)}
	if m.Content != "" {
		payload["content"] = m.Content
	}
	if m.ToolCallID != "" {
		payload["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			calls = append(calls, map[string]any{"id": tc.ID, "name": tc.Name, "args": tc.Args})
		}
		payload["tool_calls"] = calls
	}
	data, _ := json.Marshal(payload)
	return data
}

// Final writes the result envelope — the only output in json mode, the last
// line in stream-json. A non-nil err marks an error result.
func (w *Writer) Final(transcript []messages.Message, u agent.UsageTotals, model string, numTurns int, err error) {
	ev := event{Type: "result", NumTurns: numTurns}
	usage, _ := json.Marshal(u)
	ev.Usage = usage
	if est, known := cost.Estimate(model, cost.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
	}); known {
		ev.CostUSD = &est
	}
	if err != nil {
		ev.Subtype = "error"
		ev.IsError = true
		ev.ErrorText = err.Error()
	} else {
		ev.Subtype = "success"
		ev.Result = lastAItext(transcript)
	}
	w.write(ev)
}

func (w *Writer) write(ev event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintln(w.w, string(data))
}

func lastAItext(transcript []messages.Message) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == messages.RoleAI && transcript[i].Content != "" {
			return transcript[i].Content
		}
	}
	return ""
}
