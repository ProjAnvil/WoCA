package server

import (
	"encoding/json"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/graph"
)

// TestRenderReasoningDelta locks the SSE vocabulary: a ReasoningDelta step
// becomes a "reasoning" event, text deltas stay "delta", and AI/tool updates
// keep producing tool_call / tool_result events.
func TestRenderReasoningDelta(t *testing.T) {
	run := &Run{id: "test", subs: map[*subscriber]struct{}{}}

	run.render(graph.Step{Delta: "answer text"})
	run.render(graph.Step{Delta: agent.ReasoningDelta{Text: "thinking hard"}})
	run.render(graph.Step{Update: graph.State{"messages": []messages.Message{
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "t1", Name: "Read"}}},
	}}})
	run.render(graph.Step{Update: graph.State{"messages": []messages.Message{
		{Role: messages.RoleTool, ToolCallID: "t1", Content: "file body"},
	}}})

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
