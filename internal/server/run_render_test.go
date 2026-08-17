package server

import (
	"encoding/json"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
)

// TestRenderReasoningDelta locks the SSE vocabulary: a ReasoningDelta custom
// payload becomes a "reasoning" event, text payloads stay "delta", and
// update chunks keep producing tool_call / tool_result events.
func TestRenderReasoningDelta(t *testing.T) {
	run := &Run{id: "test", subs: map[*subscriber]struct{}{}}

	run.render(lggraph.StreamChunk{Mode: lggraph.StreamCustom, Payload: "answer text"})
	run.render(lggraph.StreamChunk{Mode: lggraph.StreamCustom, Payload: agent.ReasoningDelta{Text: "thinking hard"}})
	run.render(lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": map[string]any{"messages": []messages.Message{
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "t1", Name: "Read"}}},
	}}}})
	run.render(lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"tools": map[string]any{"messages": []messages.Message{
		{Role: messages.RoleTool, ToolCallID: "t1", Content: "file body"},
	}}}})

	want := []struct {
		typ string
		key string
		val string
	}{
		{"delta", "text", "answer text"},
		{"reasoning", "text", "thinking hard"},
		{"tool_call", "name", "Read"},
		{"tool_result", "content", "file body"},
	}
	if len(run.history) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(run.history), len(want), run.history)
	}
	for i, w := range want {
		ev := run.history[i]
		if ev.Type != w.typ {
			t.Errorf("event %d type = %s, want %s", i, ev.Type, w.typ)
			continue
		}
		data, _ := json.Marshal(ev.Data)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		if m[w.key] != w.val {
			t.Errorf("event %d (%s) %s = %v, want %q", i, w.typ, w.key, m[w.key], w.val)
		}
	}
}
