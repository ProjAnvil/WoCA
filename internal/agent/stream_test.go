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

func TestTextDeltaFromContentBlocks(t *testing.T) {
	chunk := messages.Message{ContentBlocks: []messages.ContentBlock{
		messages.TextBlock{Text: "hi "},
		messages.PlainTextBlock{Text: "there"},
	}}
	if got := TextDelta(chunk); got != "hi there" {
		t.Fatalf("TextDelta = %q", got)
	}
}
