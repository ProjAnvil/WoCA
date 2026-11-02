package agent

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestMergeStreamChunksText(t *testing.T) {
	var full messages.Message
	MergeStreamChunks(&full, messages.AI("hello "))
	MergeStreamChunks(&full, messages.AI("world"))
	if full.Content != "hello world" {
		t.Fatalf("content = %q", full.Content)
	}
}

func TestMergeStreamChunksToolCalls(t *testing.T) {
	var full messages.Message
	MergeStreamChunks(&full, messages.AI(""))
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "1", Name: "Bash", Args: map[string]any{"command": "ls"}},
	}})
	// A later chunk with the same ID merges args.
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "1", Name: "Bash", Args: map[string]any{"timeout_ms": 1000}},
	}})
	if len(full.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", full.ToolCalls)
	}
	call := full.ToolCalls[0]
	if call.Args["command"] != "ls" || call.Args["timeout_ms"] != 1000 {
		t.Fatalf("merged args = %+v", call.Args)
	}
}

func TestMergeStreamChunksDropsToolCallChunkFragments(t *testing.T) {
	var full messages.Message
	// openai chat-completions per-slice chunks (tool-call-delta protocol,
	// fork v0.12+): the first slice seeds the message and carries only a
	// fragment block; later slices carry args-only fragments.
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ContentBlocks: []messages.ContentBlock{
		messages.ToolCallChunkBlock{ID: "call_1", Name: "bash", Args: `{"command":"ec`},
	}})
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ContentBlocks: []messages.ContentBlock{
		messages.ToolCallChunkBlock{Args: `ho hi"}`},
	}})
	// The terminal chunk still carries the assembled call via ToolCalls.
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "call_1", Name: "bash", Args: map[string]any{"command": "echo hi"}},
	}})
	if len(full.ContentBlocks) != 0 {
		t.Fatalf("ContentBlocks = %+v, want tool_call_chunk fragments dropped", full.ContentBlocks)
	}
	if len(full.ToolCalls) != 1 || full.ToolCalls[0].Args["command"] != "echo hi" {
		t.Fatalf("ToolCalls = %+v, want the assembled call", full.ToolCalls)
	}
}

func TestMergeStreamChunksDropsAnthropicToolUsePartials(t *testing.T) {
	var full messages.Message
	// Anthropic raw stream: content_block_start and every input_json_delta
	// yield NonStandard tool_use partial blocks; content_block_stop yields the
	// complete call in ToolCalls. The partials are redundant for the merged
	// message and replay as duplicate tool_use blocks on the wire, so only
	// the complete call (and non-tool blocks) must survive the merge.
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ContentBlocks: []messages.ContentBlock{
		messages.NonStandardContentBlock{Type: "tool_use", Value: map[string]any{
			"id": "tu_1", "name": "bash", "input": map[string]any{}, "index": 0,
		}},
		messages.ReasoningBlock{Reasoning: "thinking"},
	}})
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ContentBlocks: []messages.ContentBlock{
		messages.NonStandardContentBlock{Type: "tool_use", Value: map[string]any{
			"input": `{"command":"ls`, "index": 0,
		}},
	}})
	MergeStreamChunks(&full, messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "tu_1", Name: "bash", Args: map[string]any{"command": "ls"}},
	}})
	if len(full.ContentBlocks) != 1 {
		t.Fatalf("ContentBlocks = %+v, want only the reasoning block", full.ContentBlocks)
	}
	if _, ok := full.ContentBlocks[0].(messages.ReasoningBlock); !ok {
		t.Fatalf("ContentBlocks[0] = %T, want ReasoningBlock preserved", full.ContentBlocks[0])
	}
	if len(full.ToolCalls) != 1 || full.ToolCalls[0].ID != "tu_1" {
		t.Fatalf("ToolCalls = %+v, want the complete call", full.ToolCalls)
	}
}

func TestTextDeltaFromContentBlocks(t *testing.T) {
	chunk := messages.Message{ContentBlocks: []messages.ContentBlock{
		messages.TextBlock{Text: "hi "},
		messages.PlainTextBlock{Text: "there"},
	}}
	if got := TextDelta(chunk); got != "hi there" {
		t.Fatalf("TextDelta = %q", got)
	}
}
