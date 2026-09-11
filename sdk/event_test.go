package sdk

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/sdkout"
)

func TestMessageFromAssistantWithToolCalls(t *testing.T) {
	m := messages.Message{
		Role:    messages.RoleAI,
		Content: "thinking",
		ToolCalls: []messages.ToolCall{
			{ID: "tc1", Name: "Bash", Args: map[string]any{"command": "ls"}},
		},
	}
	got := messageFrom(m)
	if got == nil || got.Role != "assistant" || got.Content != "thinking" {
		t.Fatalf("unexpected message: %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "Bash" || got.ToolCalls[0].Args["command"] != "ls" {
		t.Fatalf("tool calls mismatch: %+v", got.ToolCalls)
	}
}

// TestWireParityWithSdkout locks the Go SDK wire format to sdkout's
// stream-json lines (consumed by eval/woca_agent.py).
func TestWireParityWithSdkout(t *testing.T) {
	var buf strings.Builder
	w, err := sdkout.New(sdkout.FormatJSON, &buf)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		{Role: messages.RoleHuman, Content: "hi"},
		{Role: messages.RoleAI, Content: "hello"},
	}
	usage := agent.UsageTotals{Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	w.Final(transcript, usage, "unknown-model", 1, nil)

	ev := resultEvent(transcript, agent.UsageTotals{
		Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
	}, "unknown-model", 1, nil)
	wire, err := ev.WireJSON()
	if err != nil {
		t.Fatal(err)
	}

	var fromSdkout, fromSDK map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &fromSdkout); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &fromSDK); err != nil {
		t.Fatal(err)
	}
	if len(fromSdkout) != len(fromSDK) {
		t.Fatalf("key count differs: sdkout=%v sdk=%v", fromSdkout, fromSDK)
	}
	for k, v := range fromSdkout {
		got, ok := fromSDK[k]
		if !ok {
			t.Fatalf("sdk wire missing key %q (sdkout=%v)", k, v)
		}
		g1, _ := json.Marshal(v)
		g2, _ := json.Marshal(got)
		if string(g1) != string(g2) {
			t.Fatalf("key %q differs: sdkout=%s sdk=%s", k, g1, g2)
		}
	}
}

// TestWireParityWithSdkoutError locks the error-path wire format: subtype,
// is_error and error must match sdkout's result envelope key for key.
func TestWireParityWithSdkoutError(t *testing.T) {
	var buf strings.Builder
	w, err := sdkout.New(sdkout.FormatJSON, &buf)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		{Role: messages.RoleHuman, Content: "hi"},
		{Role: messages.RoleAI, Content: "hello"},
	}
	usage := agent.UsageTotals{Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	runErr := errors.New("boom")
	w.Final(transcript, usage, "unknown-model", 1, runErr)

	ev := resultEvent(transcript, agent.UsageTotals{
		Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
	}, "unknown-model", 1, runErr)
	wire, err := ev.WireJSON()
	if err != nil {
		t.Fatal(err)
	}

	var fromSdkout, fromSDK map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &fromSdkout); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &fromSDK); err != nil {
		t.Fatal(err)
	}
	if len(fromSdkout) != len(fromSDK) {
		t.Fatalf("key count differs: sdkout=%v sdk=%v", fromSdkout, fromSDK)
	}
	for k, v := range fromSdkout {
		got, ok := fromSDK[k]
		if !ok {
			t.Fatalf("sdk wire missing key %q (sdkout=%v)", k, v)
		}
		g1, _ := json.Marshal(v)
		g2, _ := json.Marshal(got)
		if string(g1) != string(g2) {
			t.Fatalf("key %q differs: sdkout=%s sdk=%s", k, g1, g2)
		}
	}
	if fromSDK["subtype"] != "error" || fromSDK["is_error"] != true || fromSDK["error"] != "boom" {
		t.Fatalf("error fields mismatch: %v", fromSDK)
	}
}
