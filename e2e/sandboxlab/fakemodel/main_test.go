package main

// The wire-shape contract between this scripted fake and the real client
// the worker drives (langchain-golang partners/anthropic): pin the SSE
// script by streaming through the actual ChatModel. If the partner's
// parser changes, this fails before the docker e2e ever boots.

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/modelconfig"
	"github.com/projanvil/langchain-golang/partners/anthropic"
)

// startFake mounts the scripted handler on a throwaway listener.
func startFake(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler())
	t.Cleanup(srv.Close)
	return srv
}

// collectStream drains the model's chunk stream into the aggregated
// message, mirroring the agent's MergeStreamChunks semantics (internal/
// agent/stream.go): text appends, tool calls merge by ID.
func collectStream(t *testing.T, m anthropic.ChatModel, input []messages.Message) messages.Message {
	t.Helper()
	stream, err := m.Stream(context.Background(), input)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()
	var full messages.Message
	for {
		chunk, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			return full
		}
		if full.Role == "" {
			full = chunk
			continue
		}
		full.Content += chunk.Content
		full.ToolCalls = append(full.ToolCalls, chunk.ToolCalls...)
		if chunk.UsageMetadata.TotalTokens > 0 {
			full.UsageMetadata = chunk.UsageMetadata
		}
	}
}

func TestScriptAgainstAnthropicClient(t *testing.T) {
	srv := startFake(t)
	m := anthropic.NewChatModel(
		modelconfig.WithModel("fake-claude"),
		modelconfig.WithAPIKey("sandboxlab"),
		modelconfig.WithBaseURL(srv.URL),
	)

	// Turn 1 — the script answers with the Write tool call.
	first := collectStream(t, m, []messages.Message{messages.Human("write the file")})
	if len(first.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (msg: %+v)", len(first.ToolCalls), first)
	}
	call := first.ToolCalls[0]
	if call.Name != "Write" || call.ID != "toolu_fakemodel" {
		t.Fatalf("tool call = %+v, want Write/toolu_fakemodel", call)
	}
	if call.Args["file_path"] != "out.txt" || call.Args["content"] != "sandbox-side" {
		t.Fatalf("write args = %+v", call.Args)
	}

	// Turn 2 — after the tool result, the script answers with text.
	second := collectStream(t, m, []messages.Message{
		messages.Human("write the file"),
		first,
		messages.Tool(call.ID, "ok"),
	})
	if second.Content != "sandbox run complete" {
		t.Fatalf("final content = %q, want %q", second.Content, "sandbox run complete")
	}
}
