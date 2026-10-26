package main

// The wire-shape contract between this scripted fake and the real client
// the worker drives (langchain-golang partners/anthropic): pin the SSE
// script by streaming through the actual ChatModel. If the partner's
// parser changes, this fails before the docker e2e ever boots.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// TestDemoScriptsAgainstAnthropicClient pins the full-stack demo
// scenarios (compose profile "demo"): each token's turn ladder must
// stream as tool calls (Bash/Write) until the final text turn, through
// the same real client the scheduler's agent drives.
func TestDemoScriptsAgainstAnthropicClient(t *testing.T) {
	srv := startFake(t)
	m := anthropic.NewChatModel(
		modelconfig.WithModel("fake-claude"),
		modelconfig.WithAPIKey("demo"),
		modelconfig.WithBaseURL(srv.URL),
	)

	tests := []struct {
		name     string
		prompt   string
		toolName string // turn-1 tool
		argsKey  string // argument field asserted on turn 1
		argsHas  string // substring the argument value must contain
		final    string
	}{
		{"build", "woca-demo:build round one", "Write", "file_path", "out.txt", "sandbox demo round one complete"},
		{"continue", "woca-demo:continue round two", "Bash", "command", "round2.txt", "sandbox demo round two complete"},
		{"long", "woca-demo:long run", "Bash", "command", "img.txt", "sandbox demo long run complete"},
		{"marker", "woca-demo:marker run", "Bash", "command", "/marker", "sandbox demo marker round complete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn1 := collectStream(t, m, []messages.Message{messages.Human(tt.prompt)})
			if len(turn1.ToolCalls) != 1 {
				t.Fatalf("turn 1 tool calls = %d, want 1 (msg: %+v)", len(turn1.ToolCalls), turn1)
			}
			call := turn1.ToolCalls[0]
			if call.Name != tt.toolName {
				t.Fatalf("turn 1 tool = %q, want %q", call.Name, tt.toolName)
			}
			raw, _ := json.Marshal(call.Args)
			if !strings.Contains(string(raw), tt.argsKey) || !strings.Contains(string(raw), tt.argsHas) {
				t.Fatalf("turn 1 args = %s, want %q mentioning %q", raw, tt.argsKey, tt.argsHas)
			}

			// Turn 2 after one tool result: the build script has a third
			// step (another tool call); every other scenario ends in text.
			turn2 := collectStream(t, m, []messages.Message{
				messages.Human(tt.prompt),
				turn1,
				messages.Tool(call.ID, "ok"),
			})
			if tt.name == "build" {
				if len(turn2.ToolCalls) != 1 || turn2.ToolCalls[0].Name != "Bash" {
					t.Fatalf("build turn 2 = %+v, want one Bash call", turn2)
				}
				// Turn 3 after the second tool result ends the run.
				turn3 := collectStream(t, m, []messages.Message{
					messages.Human(tt.prompt),
					turn1,
					messages.Tool(call.ID, "ok"),
					turn2,
					messages.Tool(turn2.ToolCalls[0].ID, "ok"),
				})
				if turn3.Content != tt.final || len(turn3.ToolCalls) != 0 {
					t.Fatalf("build turn 3 = %+v, want final text %q", turn3, tt.final)
				}
				return
			}
			if turn2.Content != tt.final || len(turn2.ToolCalls) != 0 {
				t.Fatalf("turn 2 = %+v, want final text %q", turn2, tt.final)
			}
		})
	}
}

// TestCountToolResults pins the turn discriminator on the raw wire body:
// string content, block arrays and mixed histories must all count right.
func TestCountToolResults(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"string content only", `{"messages":[{"role":"user","content":"hi"}]}`, 0},
		{"one tool result", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]}`, 1},
		{"mixed blocks", `{"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result"},{"type":"tool_result"}]}]}`, 2},
		{"several messages", `{"messages":[{"content":"a"},{"content":[{"type":"tool_result"}]},{"content":[{"type":"tool_result"}]}]}`, 2},
		{"malformed", `not json`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countToolResults([]byte(tt.body)); got != tt.want {
				t.Fatalf("countToolResults = %d, want %d", got, tt.want)
			}
		})
	}
}
